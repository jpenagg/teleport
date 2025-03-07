/*
Copyright 2021 Gravitational, Inc.

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

package main

import (
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/ghodss/yaml"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
)

// onAppLogin implements "tsh app login" command.
func onAppLogin(cf *CLIConf) error {
	tc, err := makeClient(cf, false)
	if err != nil {
		return trace.Wrap(err)
	}
	app, err := getRegisteredApp(cf, tc)
	if err != nil {
		return trace.Wrap(err)
	}
	profile, err := client.StatusCurrent(cf.HomePath, cf.Proxy, cf.IdentityFileIn)
	if err != nil {
		return trace.Wrap(err)
	}

	rootCluster, err := tc.RootClusterName()
	if err != nil {
		return trace.Wrap(err)
	}

	var arn string
	if app.IsAWSConsole() {
		var err error
		arn, err = getARNFromFlags(cf, profile)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	ws, err := tc.CreateAppSession(cf.Context, types.CreateAppSessionRequest{
		Username:    tc.Username,
		PublicAddr:  app.GetPublicAddr(),
		ClusterName: tc.SiteName,
		AWSRoleARN:  arn,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	err = tc.ReissueUserCerts(cf.Context, client.CertCacheKeep, client.ReissueParams{
		RouteToCluster: tc.SiteName,
		RouteToApp: proto.RouteToApp{
			Name:        app.GetName(),
			SessionID:   ws.GetName(),
			PublicAddr:  app.GetPublicAddr(),
			ClusterName: tc.SiteName,
			AWSRoleARN:  arn,
		},
		AccessRequests: profile.ActiveRequests.AccessRequests,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	if err := tc.SaveProfile(cf.HomePath, true); err != nil {
		return trace.Wrap(err)
	}
	if app.IsAWSConsole() {
		return awsCliTpl.Execute(os.Stdout, map[string]string{
			"awsAppName": app.GetName(),
			"awsCmd":     "s3 ls",
		})
	}
	curlCmd, err := formatAppConfig(tc, profile, app.GetName(), app.GetPublicAddr(), appFormatCURL, rootCluster)
	if err != nil {
		return trace.Wrap(err)
	}
	return appLoginTpl.Execute(os.Stdout, map[string]string{
		"appName": app.GetName(),
		"curlCmd": curlCmd,
	})
}

// appLoginTpl is the message that gets printed to a user upon successful app login.
var appLoginTpl = template.Must(template.New("").Parse(
	`Logged into app {{.appName}}. Example curl command:

{{.curlCmd}}
`))

// awsCliTpl is the message that gets printed to a user upon successful aws app login.
var awsCliTpl = template.Must(template.New("").Parse(
	`Logged into AWS app {{.awsAppName}}. Example AWS cli command:

tsh aws {{.awsCmd}}
`))

// getRegisteredApp returns the registered application with the specified name.
func getRegisteredApp(cf *CLIConf, tc *client.TeleportClient) (app types.Application, err error) {
	var apps []types.Application
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		allApps, err := tc.ListApps(cf.Context, &proto.ListResourcesRequest{
			Namespace:           tc.Namespace,
			PredicateExpression: fmt.Sprintf(`name == "%s"`, cf.AppName),
		})
		// Kept for fallback in case older auth does not apply filters.
		// DELETE IN 11.0.0
		for _, a := range allApps {
			if a.GetName() == cf.AppName {
				apps = append(apps, a)
				return nil
			}
		}
		return trace.Wrap(err)
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(apps) == 0 {
		return nil, trace.NotFound("app %q not found, use `tsh app ls` to see registered apps", cf.AppName)
	}
	return apps[0], nil
}

// onAppLogout implements "tsh app logout" command.
func onAppLogout(cf *CLIConf) error {
	tc, err := makeClient(cf, false)
	if err != nil {
		return trace.Wrap(err)
	}
	profile, err := client.StatusCurrent(cf.HomePath, cf.Proxy, cf.IdentityFileIn)
	if err != nil {
		return trace.Wrap(err)
	}
	var logout []tlsca.RouteToApp
	// If app name wasn't given on the command line, log out of all.
	if cf.AppName == "" {
		logout = profile.Apps
	} else {
		for _, app := range profile.Apps {
			if app.Name == cf.AppName {
				logout = append(logout, app)
			}
		}
		if len(logout) == 0 {
			return trace.BadParameter("not logged into app %q",
				cf.AppName)
		}
	}
	for _, app := range logout {
		err = tc.DeleteAppSession(cf.Context, app.SessionID)
		if err != nil && !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
		err = tc.LogoutApp(app.Name)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	if len(logout) == 1 {
		fmt.Printf("Logged out of app %q\n", logout[0].Name)
	} else {
		fmt.Println("Logged out of all apps")
	}
	return nil
}

// onAppConfig implements "tsh app config" command.
func onAppConfig(cf *CLIConf) error {
	tc, err := makeClient(cf, false)
	if err != nil {
		return trace.Wrap(err)
	}
	profile, err := client.StatusCurrent(cf.HomePath, cf.Proxy, cf.IdentityFileIn)
	if err != nil {
		return trace.Wrap(err)
	}
	app, err := pickActiveApp(cf)
	if err != nil {
		return trace.Wrap(err)
	}
	conf, err := formatAppConfig(tc, profile, app.Name, app.PublicAddr, cf.Format, "")
	if err != nil {
		return trace.Wrap(err)
	}
	fmt.Print(conf)
	return nil
}

func formatAppConfig(tc *client.TeleportClient, profile *client.ProfileStatus, appName, appPublicAddr, format, cluster string) (string, error) {
	var uri string
	if port := tc.WebProxyPort(); port == teleport.StandardHTTPSPort {
		uri = fmt.Sprintf("https://%v", appPublicAddr)
	} else {
		uri = fmt.Sprintf("https://%v:%v", appPublicAddr, port)
	}
	curlCmd := fmt.Sprintf(`curl \
  --cacert %v \
  --cert %v \
  --key %v \
  %v`,
		profile.CACertPathForCluster(cluster),
		profile.AppCertPath(appName),
		profile.KeyPath(),
		uri)
	format = strings.ToLower(format)
	switch format {
	case appFormatURI:
		return uri, nil
	case appFormatCA:
		return profile.CACertPathForCluster(cluster), nil
	case appFormatCert:
		return profile.AppCertPath(appName), nil
	case appFormatKey:
		return profile.KeyPath(), nil
	case appFormatCURL:
		return curlCmd, nil
	case appFormatJSON, appFormatYAML:
		appConfig := &appConfigInfo{
			appName, uri, profile.CACertPathForCluster(cluster),
			profile.AppCertPath(appName), profile.KeyPath(), curlCmd,
		}
		out, err := serializeAppConfig(appConfig, format)
		if err != nil {
			return "", trace.Wrap(err)
		}
		return fmt.Sprintf("%s\n", out), nil
	}
	return fmt.Sprintf(`Name:      %v
URI:       %v
CA:        %v
Cert:      %v
Key:       %v
`, appName, uri, profile.CACertPathForCluster(cluster),
		profile.AppCertPath(appName), profile.KeyPath()), nil
}

type appConfigInfo struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	CA   string `json:"ca"`
	Cert string `json:"cert"`
	Key  string `json:"key"`
	Curl string `json:"curl"`
}

func serializeAppConfig(configInfo *appConfigInfo, format string) (string, error) {
	var out []byte
	var err error
	if format == appFormatJSON {
		out, err = utils.FastMarshalIndent(configInfo, "", "  ")
	} else {
		out, err = yaml.Marshal(configInfo)
	}
	return string(out), trace.Wrap(err)
}

// pickActiveApp returns the app the current profile is logged into.
//
// If logged into multiple apps, returns an error unless one was specified
// explicitly on CLI.
func pickActiveApp(cf *CLIConf) (*tlsca.RouteToApp, error) {
	profile, err := client.StatusCurrent(cf.HomePath, cf.Proxy, cf.IdentityFileIn)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(profile.Apps) == 0 {
		return nil, trace.NotFound("please login using 'tsh app login' first")
	}
	name := cf.AppName
	if name == "" {
		apps := profile.AppNames()
		if len(apps) > 1 {
			return nil, trace.BadParameter("multiple apps are available (%v), please specify one via CLI argument",
				strings.Join(apps, ", "))
		}
		name = apps[0]
	}
	for _, app := range profile.Apps {
		if app.Name == name {
			return &app, nil
		}
	}
	return nil, trace.NotFound("not logged into app %q", name)
}

const (
	// appFormatURI prints app URI.
	appFormatURI = "uri"
	// appFormatCA prints app CA cert path.
	appFormatCA = "ca"
	// appFormatCert prints app cert path.
	appFormatCert = "cert"
	// appFormatKey prints app key path.
	appFormatKey = "key"
	// appFormatCURL prints app curl command.
	appFormatCURL = "curl"
	// appFormatJSON prints app URI, CA cert path, cert path, key path, and curl command in JSON format.
	appFormatJSON = "json"
	// appFormatYAML prints app URI, CA cert path, cert path, key path, and curl command in YAML format.
	appFormatYAML = "yaml"
)
