/*
Copyright 2018-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"crypto/tls"
	"path/filepath"
	"strings"

	"github.com/coreos/go-semver/semver"
	"github.com/gravitational/roundtrip"
	"github.com/gravitational/trace"
	om "github.com/grpc-ecosystem/go-grpc-middleware/providers/openmetrics/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/interval"
)

// reconnectToAuthService continuously attempts to reconnect to the auth
// service until succeeds or process gets shut down
func (process *TeleportProcess) reconnectToAuthService(role types.SystemRole) (*Connector, error) {
	retry, err := utils.NewLinear(utils.LinearConfig{
		First:  utils.HalfJitter(process.Config.MaxRetryPeriod / 10),
		Step:   process.Config.MaxRetryPeriod / 5,
		Max:    process.Config.MaxRetryPeriod,
		Clock:  process.Clock,
		Jitter: utils.NewHalfJitter(),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	for {
		connector, err := process.connectToAuthService(role)
		if err == nil {
			// if connected and client is present, make sure the connector's
			// client works, by using call that should succeed at all times
			if connector.Client != nil {
				pingResponse, err := connector.Client.Ping(process.ExitContext())
				compareErr := process.authServerTooOld(&pingResponse)
				if compareErr != nil {
					return nil, trace.Wrap(compareErr)
				}

				if err == nil {
					process.setClusterFeatures(pingResponse.GetServerFeatures())
					process.log.Infof("%v: features loaded from auth server: %+v", role, pingResponse.GetServerFeatures())
					return connector, nil
				}

				process.log.Debugf("Connected client %v failed to execute test call: %v. Node or proxy credentials are out of sync.", role, err)
				if err := connector.Client.Close(); err != nil {
					process.log.Debugf("Failed to close the client: %v.", err)
				}
			}
		}
		process.log.Errorf("%v failed to establish connection to cluster: %v.", role, err)

		// Used for testing that auth service will attempt to reconnect in the provided duration.
		select {
		case process.Config.ConnectFailureC <- retry.Duration():
		default:
		}

		startedWait := process.Clock.Now()
		// Wait in between attempts, but return if teleport is shutting down
		select {
		case t := <-retry.After():
			process.log.Debugf("Retrying connection to auth server after waiting %v.", t.Sub(startedWait))
			retry.Inc()
		case <-process.ExitContext().Done():
			process.log.Infof("%v stopping connection attempts, teleport is shutting down.", role)
			return nil, ErrTeleportExited
		}
	}
}

func (process *TeleportProcess) authServerTooOld(resp *proto.PingResponse) error {
	serverVersion, err := semver.NewVersion(resp.ServerVersion)
	if err != nil {
		return trace.Wrap(err)
	}

	version := teleport.Version
	if process.Config.TeleportVersion != "" {
		version = process.Config.TeleportVersion
	}
	teleportVersion, err := semver.NewVersion(version)
	if err != nil {
		return trace.Wrap(err)
	}

	if serverVersion.Major < teleportVersion.Major {
		if process.Config.SkipVersionCheck {
			process.log.Warnf("Only versions %d and greater are supported, but auth server is version %d.", teleportVersion.Major, serverVersion.Major)
			return nil
		}
		return trace.NotImplemented("only versions %d and greater are supported, but auth server is version %d. To connect anyway pass the '--skip-version-check' flag.", teleportVersion.Major, serverVersion.Major)
	}

	return nil
}

// connectToAuthService attempts to login into the auth servers specified in the
// configuration and receive credentials.
func (process *TeleportProcess) connectToAuthService(role types.SystemRole) (*Connector, error) {
	connector, err := process.connect(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.log.Debugf("Connected client: %v", connector.ClientIdentity)
	process.addConnector(connector)

	return connector, nil
}

func (process *TeleportProcess) connect(role types.SystemRole) (conn *Connector, err error) {
	state, err := process.storage.GetState(role)
	if err != nil {
		if !trace.IsNotFound(err) {
			return nil, trace.Wrap(err)
		}
		// no state recorded - this is the first connect
		// process will try to connect with the security token.
		return process.firstTimeConnect(role)
	}
	process.log.Debugf("Connected state: %v.", state.Spec.Rotation.String())

	identity, err := process.GetIdentity(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	rotation := state.Spec.Rotation

	switch rotation.State {
	// rotation is on standby, so just use whatever is current
	case "", types.RotationStateStandby:
		// The roles of admin and auth are treated in a special way, as in this case
		// the process does not need TLS clients and can use local auth directly.
		if role == types.RoleAdmin || role == types.RoleAuth {
			return &Connector{
				ClientIdentity: identity,
				ServerIdentity: identity,
			}, nil
		}
		process.log.Infof("Connecting to the cluster %v with TLS client certificate.", identity.ClusterName)
		clt, err := process.newClient(process.Config.AuthServers, identity)
		if err != nil {
			// In the event that a user is attempting to connect a machine to
			// a different cluster it will give a cryptic warning about an
			// unknown certificate authority. Unfortunately we cannot intercept
			// this error as it comes from the http package before a request is
			// made. So provide a more user friendly error as a hint of what
			// they can do to resolve the issue.
			if strings.Contains(err.Error(), "certificate signed by unknown authority") {
				process.log.Errorf("Was this node already registered to a different cluster? To join this node to a new cluster, remove `%s` and try again", process.Config.DataDir)
			}
			return nil, trace.Wrap(err)
		}
		return &Connector{
			Client:         clt,
			ClientIdentity: identity,
			ServerIdentity: identity,
		}, nil
	case types.RotationStateInProgress:
		switch rotation.Phase {
		case types.RotationPhaseInit:
			// Both clients and servers are using old credentials,
			// this phase exists for remote clusters to propagate information about the new CA
			if role == types.RoleAdmin || role == types.RoleAuth {
				return &Connector{
					ClientIdentity: identity,
					ServerIdentity: identity,
				}, nil
			}
			clt, err := process.newClient(process.Config.AuthServers, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         clt,
				ClientIdentity: identity,
				ServerIdentity: identity,
			}, nil
		case types.RotationPhaseUpdateClients:
			// Clients should use updated credentials,
			// while servers should use old credentials to answer auth requests.
			newIdentity, err := process.storage.ReadIdentity(auth.IdentityReplacement, role)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if role == types.RoleAdmin || role == types.RoleAuth {
				return &Connector{
					ClientIdentity: newIdentity,
					ServerIdentity: identity,
				}, nil
			}
			clt, err := process.newClient(process.Config.AuthServers, newIdentity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         clt,
				ClientIdentity: newIdentity,
				ServerIdentity: identity,
			}, nil
		case types.RotationPhaseUpdateServers:
			// Servers and clients are using new identity credentials, but the
			// identity is still set up to trust the old certificate authority certificates.
			newIdentity, err := process.storage.ReadIdentity(auth.IdentityReplacement, role)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if role == types.RoleAdmin || role == types.RoleAuth {
				return &Connector{
					ClientIdentity: newIdentity,
					ServerIdentity: newIdentity,
				}, nil
			}
			clt, err := process.newClient(process.Config.AuthServers, newIdentity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         clt,
				ClientIdentity: newIdentity,
				ServerIdentity: newIdentity,
			}, nil
		case types.RotationPhaseRollback:
			// In rollback phase, clients and servers should switch back
			// to the old certificate authority-issued credentials,
			// but the new certificate authority should be trusted
			// because not all clients can update at the same time.
			if role == types.RoleAdmin || role == types.RoleAuth {
				return &Connector{
					ClientIdentity: identity,
					ServerIdentity: identity,
				}, nil
			}
			clt, err := process.newClient(process.Config.AuthServers, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         clt,
				ClientIdentity: identity,
				ServerIdentity: identity,
			}, nil
		default:
			return nil, trace.BadParameter("unsupported rotation phase: %q", rotation.Phase)
		}
	default:
		return nil, trace.BadParameter("unsupported rotation state: %q", rotation.State)
	}
}

// KeyPair is a private/public key pair
type KeyPair struct {
	// PrivateKey is a private key in PEM format
	PrivateKey []byte
	// PublicSSHKey is a public key in SSH format
	PublicSSHKey []byte
	// PublicTLSKey is a public key in X509 format
	PublicTLSKey []byte
}

func (process *TeleportProcess) deleteKeyPair(role types.SystemRole, reason string) {
	process.keyMutex.Lock()
	defer process.keyMutex.Unlock()
	process.log.Debugf("Deleted generated key pair %v %v.", role, reason)
	delete(process.keyPairs, keyPairKey{role: role, reason: reason})
}

func (process *TeleportProcess) generateKeyPair(role types.SystemRole, reason string) (*KeyPair, error) {
	process.keyMutex.Lock()
	defer process.keyMutex.Unlock()

	mapKey := keyPairKey{role: role, reason: reason}
	keyPair, ok := process.keyPairs[mapKey]
	if ok {
		process.log.Debugf("Returning existing key pair for %v %v.", role, reason)
		return &keyPair, nil
	}
	process.log.Debugf("Generating new key pair for %v %v.", role, reason)
	privPEM, pubSSH, err := native.GenerateKeyPair()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	privateKey, err := ssh.ParseRawPrivateKey(privPEM)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	pubTLS, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(privateKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	keyPair = KeyPair{PrivateKey: privPEM, PublicSSHKey: pubSSH, PublicTLSKey: pubTLS}
	process.keyPairs[mapKey] = keyPair

	return &keyPair, nil
}

// newWatcher returns a new watcher,
// either using local auth server connection or remote client
func (process *TeleportProcess) newWatcher(conn *Connector, watch types.Watch) (types.Watcher, error) {
	if conn.ClientIdentity.ID.Role == types.RoleAdmin || conn.ClientIdentity.ID.Role == types.RoleAuth {
		return process.localAuth.NewWatcher(process.ExitContext(), watch)
	}
	return conn.Client.NewWatcher(process.ExitContext(), watch)
}

// getCertAuthority returns cert authority by ID.
// In case if auth servers, the role is 'TeleportAdmin' and instead of using
// TLS client this method uses the local auth server.
func (process *TeleportProcess) getCertAuthority(conn *Connector, id types.CertAuthID, loadPrivateKeys bool) (types.CertAuthority, error) {
	if conn.ClientIdentity.ID.Role == types.RoleAdmin || conn.ClientIdentity.ID.Role == types.RoleAuth {
		return process.localAuth.GetCertAuthority(process.ExitContext(), id, loadPrivateKeys)
	}
	return conn.Client.GetCertAuthority(process.ExitContext(), id, loadPrivateKeys)
}

// reRegister receives new identity credentials for proxy, node and auth.
// In case if auth servers, the role is 'TeleportAdmin' and instead of using
// TLS client this method uses the local auth server.
func (process *TeleportProcess) reRegister(conn *Connector, additionalPrincipals []string, dnsNames []string, rotation types.Rotation) (*auth.Identity, error) {
	id := conn.ClientIdentity.ID
	if id.NodeName == "" {
		id.NodeName = process.Config.Hostname
	}
	if id.Role == types.RoleAdmin || id.Role == types.RoleAuth {
		return auth.GenerateIdentity(process.localAuth, id, additionalPrincipals, dnsNames)
	}
	const reason = "re-register"
	keyPair, err := process.generateKeyPair(id.Role, reason)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	identity, err := auth.ReRegister(auth.ReRegisterParams{
		Client:               conn.Client,
		ID:                   id,
		AdditionalPrincipals: additionalPrincipals,
		PrivateKey:           keyPair.PrivateKey,
		PublicTLSKey:         keyPair.PublicTLSKey,
		PublicSSHKey:         keyPair.PublicSSHKey,
		DNSNames:             dnsNames,
		Rotation:             rotation,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.deleteKeyPair(id.Role, reason)
	return identity, nil
}

func (process *TeleportProcess) firstTimeConnect(role types.SystemRole) (*Connector, error) {
	id := auth.IdentityID{
		Role:     role,
		HostUUID: process.Config.HostUUID,
		NodeName: process.Config.Hostname,
	}
	additionalPrincipals, dnsNames, err := process.getAdditionalPrincipals(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var identity *auth.Identity
	if process.getLocalAuth() != nil {
		// Auth service is on the same host, no need to go though the invitation
		// procedure.
		process.log.Debugf("This server has local Auth server started, using it to add role to the cluster.")
		identity, err = auth.LocalRegister(id, process.getLocalAuth(), additionalPrincipals, dnsNames, process.Config.AdvertiseIP)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		// Auth server is remote, so we need a provisioning token.
		if process.Config.Token == "" {
			return nil, trace.BadParameter("%v must join a cluster and needs a provisioning token", role)
		}

		process.log.Infof("Joining the cluster with a secure token.")
		const reason = "first-time-connect"
		keyPair, err := process.generateKeyPair(role, reason)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		certs, err := auth.Register(auth.RegisterParams{
			Token:                process.Config.Token,
			ID:                   id,
			Servers:              process.Config.AuthServers,
			AdditionalPrincipals: additionalPrincipals,
			DNSNames:             dnsNames,
			PublicTLSKey:         keyPair.PublicTLSKey,
			PublicSSHKey:         keyPair.PublicSSHKey,
			CipherSuites:         process.Config.CipherSuites,
			CAPins:               process.Config.CAPins,
			CAPath:               filepath.Join(defaults.DataDir, defaults.CACertFile),
			GetHostCredentials:   client.HostCredentials,
			Clock:                process.Clock,
			JoinMethod:           process.Config.JoinMethod,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		identity, err = auth.ReadIdentityFromKeyPair(keyPair.PrivateKey, certs)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		process.deleteKeyPair(role, reason)
	}

	process.log.Infof("%v has obtained credentials to connect to the cluster.", role)
	var connector *Connector
	if role == types.RoleAdmin || role == types.RoleAuth {
		connector = &Connector{
			ClientIdentity: identity,
			ServerIdentity: identity,
		}
	} else {
		clt, err := process.newClient(process.Config.AuthServers, identity)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		connector = &Connector{
			ClientIdentity: identity,
			ServerIdentity: identity,
			Client:         clt,
		}
	}

	// Sync local rotation state to match the remote rotation state.
	ca, err := process.getCertAuthority(connector, types.CertAuthID{
		DomainName: connector.ClientIdentity.ClusterName,
		Type:       types.HostCA,
	}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = process.storage.WriteIdentity(auth.IdentityCurrent, *identity)
	if err != nil {
		process.log.Warningf("Failed to write %v identity: %v.", role, err)
	}

	err = process.storage.WriteState(role, auth.StateV2{
		Spec: auth.StateSpecV2{
			Rotation: ca.GetRotation(),
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.log.Infof("The process successfully wrote the credentials and state of %v to the disk.", role)
	return connector, nil
}

// periodicSyncRotationState checks rotation state periodically and
// takes action if necessary
func (process *TeleportProcess) periodicSyncRotationState() error {
	// start rotation only after teleport process has started
	eventC := make(chan Event, 1)
	process.WaitForEvent(process.ExitContext(), TeleportReadyEvent, eventC)
	select {
	case <-eventC:
		process.log.Infof("The new service has started successfully. Starting syncing rotation status with period %v.", process.Config.PollingPeriod)
	case <-process.GracefulExitContext().Done():
		return nil
	}

	periodic := interval.New(interval.Config{
		Duration:      process.Config.RotationConnectionInterval,
		FirstDuration: utils.HalfJitter(process.Config.RotationConnectionInterval),
		Jitter:        utils.NewSeventhJitter(),
	})
	defer periodic.Stop()

	for {
		err := process.syncRotationStateCycle()
		if err == nil {
			return nil
		}

		process.log.Warningf("Sync rotation state cycle failed. Retrying in ~%v", process.Config.RotationConnectionInterval)

		select {
		case <-periodic.Next():
		case <-process.GracefulExitContext().Done():
			return nil
		}
	}
}

// syncRotationCycle executes a rotation cycle that returns:
//
// * nil whenever rotation state leads to teleport reload event
// * error whenever rotation cycle has to be restarted
//
// the function accepts extra delay timer extraDelay in case if parent
// function needs a
func (process *TeleportProcess) syncRotationStateCycle() error {
	connectors := process.getConnectors()
	if len(connectors) == 0 {
		return trace.BadParameter("no connectors found")
	}
	// it is important to use the same view of the certificate authority
	// for all internal services at the same time, so that the same
	// procedure will be applied at the same time for multiple service process
	// and no internal services is left behind.
	conn := connectors[0]

	status, err := process.syncRotationStateAndBroadcast(conn)
	if err != nil {
		return trace.Wrap(err)
	}
	if status.needsReload {
		return nil
	}

	watcher, err := process.newWatcher(conn, types.Watch{Kinds: []types.WatchKind{{
		Kind: types.KindCertAuthority,
		Filter: types.CertAuthorityFilter{
			types.HostCA: conn.ClientIdentity.ClusterName,
		}.IntoMap(),
	}}})
	if err != nil {
		return trace.Wrap(err)
	}
	defer watcher.Close()

	periodic := interval.New(interval.Config{
		Duration:      process.Config.PollingPeriod,
		FirstDuration: utils.HalfJitter(process.Config.PollingPeriod),
		Jitter:        utils.NewSeventhJitter(),
	})
	defer periodic.Stop()
	for {
		select {
		case event := <-watcher.Events():
			if event.Type == types.OpInit || event.Type == types.OpDelete {
				continue
			}
			ca, ok := event.Resource.(types.CertAuthority)
			if !ok {
				process.log.Debugf("Skipping event %v for %v", event.Type, event.Resource.GetName())
				continue
			}
			if ca.GetType() != types.HostCA || ca.GetClusterName() != conn.ClientIdentity.ClusterName {
				process.log.Debugf("Skipping event for %v %v", ca.GetType(), ca.GetClusterName())
				continue
			}
			if status.ca.GetResourceID() > ca.GetResourceID() {
				process.log.Debugf("Skipping stale event %v, latest object version is %v.", ca.GetResourceID(), status.ca.GetResourceID())
				continue
			}
			status, err := process.syncRotationStateAndBroadcast(conn)
			if err != nil {
				return trace.Wrap(err)
			}
			if status.needsReload {
				return nil
			}
		case <-watcher.Done():
			return trace.ConnectionProblem(watcher.Error(), "watcher has disconnected")
		case <-periodic.Next():
			status, err := process.syncRotationStateAndBroadcast(conn)
			if err != nil {
				return trace.Wrap(err)
			}
			if status.needsReload {
				return nil
			}
		case <-process.GracefulExitContext().Done():
			return nil
		}
	}
}

// syncRotationStateAndBroadcast syncs rotation state and broadcasts events
// when phase has been changed or reload happened
func (process *TeleportProcess) syncRotationStateAndBroadcast(conn *Connector) (*rotationStatus, error) {
	status, err := process.syncRotationState(conn)
	if err != nil {
		if trace.IsConnectionProblem(err) {
			process.log.Warningf("Connection problem: sync rotation state: %v.", err)
		} else {
			process.log.Warningf("Failed to sync rotation state: %v.", err)
		}
		return nil, trace.Wrap(err)
	}

	if status.phaseChanged || status.needsReload {
		process.log.Debugf("Sync rotation state detected cert authority reload phase update.")
	}
	if status.phaseChanged {
		process.BroadcastEvent(Event{Name: TeleportPhaseChangeEvent})
	}
	if status.needsReload {
		process.log.Debugf("Triggering reload process.")
		process.BroadcastEvent(Event{Name: TeleportReloadEvent})
	}
	return status, nil
}

// syncRotationState compares cluster rotation state with the state of
// internal services and performs the rotation if necessary.
func (process *TeleportProcess) syncRotationState(conn *Connector) (*rotationStatus, error) {
	connectors := process.getConnectors()
	ca, err := process.getCertAuthority(conn, types.CertAuthID{
		DomainName: conn.ClientIdentity.ClusterName,
		Type:       types.HostCA,
	}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var status rotationStatus
	status.ca = ca
	for _, conn := range connectors {
		serviceStatus, err := process.syncServiceRotationState(ca, conn)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if serviceStatus.needsReload {
			status.needsReload = true
		}
		if serviceStatus.phaseChanged {
			status.phaseChanged = true
		}
	}
	return &status, nil
}

// syncServiceRotationState syncs up rotation state for internal services (Auth, Proxy, Node) and
// if necessary, updates credentials. Returns true if the service will need to reload.
func (process *TeleportProcess) syncServiceRotationState(ca types.CertAuthority, conn *Connector) (*rotationStatus, error) {
	state, err := process.storage.GetState(conn.ClientIdentity.ID.Role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return process.rotate(conn, *state, ca.GetRotation())
}

type rotationStatus struct {
	// needsReload means that phase has been updated
	// and teleport process has to reload
	needsReload bool
	// phaseChanged means that teleport phase has been updated,
	// but teleport does not need reload
	phaseChanged bool
	// ca is the certificate authority
	// fetched during status check
	ca types.CertAuthority
}

// checkServerIdentity returns a boolean that indicates the host certificate
// needs to be regenerated.
func checkServerIdentity(conn *Connector, additionalPrincipals []string, dnsNames []string, log logrus.FieldLogger) bool {
	var principalsChanged bool
	var dnsNamesChanged bool

	// Remove 0.0.0.0 (meaning advertise_ip has not) if it exists in the list of
	// principals. The 0.0.0.0 values tells the auth server to "guess" the nodes
	// IP. If 0.0.0.0 is not removed, a check is performed if it exists in the
	// list of principals in the certificate. Since it never exists in the list
	// of principals (auth server will always remove it before issuing a
	// certificate) regeneration is always requested.
	principalsToCheck := utils.RemoveFromSlice(additionalPrincipals, defaults.AnyAddress)

	// If advertise_ip, public_addr, or listen_addr in file configuration were
	// updated, the list of principals (SSH) or DNS names (TLS) on the
	// certificate need to be updated.
	if len(additionalPrincipals) != 0 && !conn.ServerIdentity.HasPrincipals(principalsToCheck) {
		principalsChanged = true
		log.Debugf("Rotation in progress, adding %v to SSH principals %v.",
			additionalPrincipals, conn.ServerIdentity.Cert.ValidPrincipals)
	}
	if len(dnsNames) != 0 && !conn.ServerIdentity.HasDNSNames(dnsNames) {
		dnsNamesChanged = true
		log.Debugf("Rotation in progress, adding %v to x590 DNS names in SAN %v.",
			dnsNames, conn.ServerIdentity.XCert.DNSNames)
	}

	return principalsChanged || dnsNamesChanged
}

// rotate is called to check if rotation should be triggered.
func (process *TeleportProcess) rotate(conn *Connector, localState auth.StateV2, remote types.Rotation) (*rotationStatus, error) {
	id := conn.ClientIdentity.ID
	local := localState.Spec.Rotation

	additionalPrincipals, dnsNames, err := process.getAdditionalPrincipals(id.Role)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Check if any of the SSH principals or TLS DNS names have changed and the
	// host credentials need to be regenerated.
	regenerateCertificate := checkServerIdentity(conn, additionalPrincipals, dnsNames, process.log)

	// If the local state matches remote state and neither principals or DNS
	// names changed, nothing to do. CA is in sync.
	if local.Matches(remote) && !regenerateCertificate {
		return &rotationStatus{}, nil
	}

	storage := process.storage

	const outOfSync = "%v and cluster rotation state (%v) is out of sync with local (%v). Clear local state and re-register this %v."

	writeStateAndIdentity := func(name string, identity *auth.Identity) error {
		err = storage.WriteIdentity(name, *identity)
		if err != nil {
			return trace.Wrap(err)
		}
		localState.Spec.Rotation = remote
		err = storage.WriteState(id.Role, localState)
		if err != nil {
			return trace.Wrap(err)
		}
		return nil
	}

	switch remote.State {
	case "", types.RotationStateStandby:
		switch local.State {
		// There is nothing to do, it could happen
		// that the old node came up and missed the whole rotation
		// rollback cycle.
		case "", types.RotationStateStandby:
			if regenerateCertificate {
				process.log.Infof("Service %v has updated principals to %q, DNS Names to %q, going to request new principals and update.", id.Role, additionalPrincipals, dnsNames)
				identity, err := process.reRegister(conn, additionalPrincipals, dnsNames, remote)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				err = storage.WriteIdentity(auth.IdentityCurrent, *identity)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				return &rotationStatus{needsReload: true}, nil
			}
			return &rotationStatus{}, nil
		case types.RotationStateInProgress:
			// Rollback phase has been completed, all services
			// will receive new identities.
			if local.Phase != types.RotationPhaseRollback && local.CurrentID != remote.CurrentID {
				return nil, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			identity, err := process.reRegister(conn, additionalPrincipals, dnsNames, remote)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityCurrent, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &rotationStatus{needsReload: true}, nil
		default:
			return nil, trace.BadParameter("unsupported state: %q", localState)
		}
	case types.RotationStateInProgress:
		switch remote.Phase {
		case types.RotationPhaseStandby, "":
			// There is nothing to do.
			return &rotationStatus{}, nil
		case types.RotationPhaseInit:
			// Only allow transition in case if local rotation state is standby
			// so this server is in the "clean" state.
			if local.State != types.RotationStateStandby && local.State != "" {
				return nil, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			// only update local phase, there is no need to reload
			localState.Spec.Rotation = remote
			err = storage.WriteState(id.Role, localState)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &rotationStatus{phaseChanged: true}, nil
		case types.RotationPhaseUpdateClients:
			// Allow transition to this phase only if the previous
			// phase was "Init".
			if local.Phase != types.RotationPhaseInit && local.CurrentID != remote.CurrentID {
				return nil, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			identity, err := process.reRegister(conn, additionalPrincipals, dnsNames, remote)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			process.log.Debugf("Re-registered, received new identity %v.", identity)
			err = writeStateAndIdentity(auth.IdentityReplacement, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			// Require reload of teleport process to update client and servers.
			return &rotationStatus{needsReload: true}, nil
		case types.RotationPhaseUpdateServers:
			// Allow transition to this phase only if the previous
			// phase was "Update clients".
			if local.Phase != types.RotationPhaseUpdateClients && local.CurrentID != remote.CurrentID {
				return nil, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			// Write the replacement identity as a current identity and reload the server.
			replacement, err := storage.ReadIdentity(auth.IdentityReplacement, id.Role)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityCurrent, replacement)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			// Require reload of teleport process to update servers.
			return &rotationStatus{needsReload: true}, nil
		case types.RotationPhaseRollback:
			// Allow transition to this phase from any other local phase
			// because it will be widely used to recover cluster state to
			// the previously valid state, client will re-register to receive
			// credentials signed by the "old" CA.
			identity, err := process.reRegister(conn, additionalPrincipals, dnsNames, remote)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityCurrent, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			// Require reload of teleport process to update servers.
			return &rotationStatus{needsReload: true}, nil
		default:
			return nil, trace.BadParameter("unsupported phase: %q", remote.Phase)
		}
	default:
		return nil, trace.BadParameter("unsupported state: %q", remote.State)
	}
}

// newClient attempts to connect directly to the Auth Server. If it fails, it
// falls back to trying to connect to the Auth Server through the proxy.
// The proxy address might be configured in process environment as apidefaults.TunnelPublicAddrEnvar
// in which case, no attempt at discovering the reverse tunnel address is made.
func (process *TeleportProcess) newClient(authServers []utils.NetAddr, identity *auth.Identity) (*auth.Client, error) {
	tlsConfig, err := identity.TLSConfig(process.Config.CipherSuites)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	logger := process.log.WithField("auth-addrs", utils.NetAddrsToStrings(authServers))
	logger.Debug("Attempting to connect to Auth Server directly.")
	directClient, directErr := process.newClientDirect(authServers, tlsConfig, identity.ID.Role)
	if directErr == nil {
		logger.Debug("Connected to Auth Server with direct connection.")
		return directClient, nil
	}
	logger.Debug("Failed to connect to Auth Server directly.")
	// store err in directLogger, only log it if tunnel dial fails.
	directErrLogger := logger.WithError(directErr)

	// Don't attempt to connect through a tunnel as a proxy or auth server.
	if identity.ID.Role == types.RoleAuth || identity.ID.Role == types.RoleProxy {
		return nil, trace.Wrap(directErr)
	}

	logger.Debug("Attempting to discover reverse tunnel address.")

	logger.Debug("Attempting to connect to Auth Server through tunnel.")
	sshClientConfig, err := identity.SSHClientConfig(process.Config.FIPS)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tunnelClient, err := process.newClientThroughTunnel(authServers, tlsConfig, sshClientConfig)
	if err != nil {
		directErrLogger.Debug("Failed to connect to Auth Server directly.")
		logger.WithError(err).Debug("Failed to connect to Auth Server through tunnel.")
		return nil, trace.WrapWithMessage(
			trace.NewAggregate(directErr, err),
			trace.Errorf("Failed to connect to Auth Server directly or over tunnel, no methods remaining."))
	}

	logger.Debug("Connected to Auth Server through tunnel.")
	return tunnelClient, nil
}

func (process *TeleportProcess) newClientThroughTunnel(authServers []utils.NetAddr, tlsConfig *tls.Config, sshConfig *ssh.ClientConfig) (*auth.Client, error) {
	resolver := reversetunnel.WebClientResolver(process.ExitContext(), authServers, lib.IsInsecureDevMode())

	resolver, err := reversetunnel.CachingResolver(resolver, process.Clock)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	dialer, err := reversetunnel.NewTunnelAuthDialer(reversetunnel.TunnelAuthDialerConfig{
		Resolver:              resolver,
		ClientConfig:          sshConfig,
		Log:                   process.log,
		InsecureSkipTLSVerify: lib.IsInsecureDevMode(),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clt, err := auth.NewClient(apiclient.Config{
		Dialer: dialer,
		Credentials: []apiclient.Credentials{
			apiclient.LoadTLS(tlsConfig),
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Check connectivity to cluster. If the request fails, unwrap the error to
	// get the underlying error.
	_, err = clt.GetLocalClusterName()
	if err != nil {
		if err2 := clt.Close(); err2 != nil {
			process.log.WithError(err2).Warn("Failed to close Auth Server tunnel client.")
		}
		return nil, trace.Unwrap(err)
	}

	return clt, nil
}

func (process *TeleportProcess) newClientDirect(authServers []utils.NetAddr, tlsConfig *tls.Config, role types.SystemRole) (*auth.Client, error) {
	var cltParams []roundtrip.ClientParam
	if process.Config.ClientTimeout != 0 {
		cltParams = []roundtrip.ClientParam{auth.ClientTimeout(process.Config.ClientTimeout)}
	}

	var dialOpts []grpc.DialOption
	if role == types.RoleProxy {
		grpcMetrics := utils.CreateGRPCClientMetrics(process.Config.Metrics.GRPCClientLatency, prometheus.Labels{teleport.TagClient: "teleport-proxy"})
		if err := utils.RegisterPrometheusCollectors(grpcMetrics); err != nil {
			return nil, trace.Wrap(err)
		}
		dialOpts = append(dialOpts, []grpc.DialOption{
			grpc.WithUnaryInterceptor(om.UnaryClientInterceptor(grpcMetrics)),
			grpc.WithStreamInterceptor(om.StreamClientInterceptor(grpcMetrics)),
		}...)
	}

	clt, err := auth.NewClient(apiclient.Config{
		Addrs: utils.NetAddrsToStrings(authServers),
		Credentials: []apiclient.Credentials{
			apiclient.LoadTLS(tlsConfig),
		},
		DialOpts: dialOpts,
		// Deliberately ignore HTTP proxies for backwards compatibility.
		IgnoreHTTPProxy: true,
	}, cltParams...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if _, err := clt.GetLocalClusterName(); err != nil {
		if err2 := clt.Close(); err2 != nil {
			process.log.WithError(err2).Warn("Failed to close direct Auth Server client.")
		}
		return nil, trace.Wrap(err)
	}

	return clt, nil
}
