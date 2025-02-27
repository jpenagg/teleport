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

package cloud

import (
	"context"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/common"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/elasticache"
	"github.com/aws/aws-sdk-go/service/elasticache/elasticacheiface"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/aws/aws-sdk-go/service/rds/rdsiface"
	"github.com/aws/aws-sdk-go/service/redshift"
	"github.com/aws/aws-sdk-go/service/redshift/redshiftiface"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// MetadataConfig is the cloud metadata service config.
type MetadataConfig struct {
	// Clients is an interface for retrieving cloud clients.
	Clients common.CloudClients
}

// Check validates the metadata service config.
func (c *MetadataConfig) Check() error {
	if c.Clients == nil {
		c.Clients = common.NewCloudClients()
	}
	return nil
}

// Metadata is a service that fetches cloud databases metadata.
type Metadata struct {
	cfg MetadataConfig
	log logrus.FieldLogger
}

// NewMetadata returns a new cloud metadata service.
func NewMetadata(config MetadataConfig) (*Metadata, error) {
	if err := config.Check(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &Metadata{
		cfg: config,
		log: logrus.WithField(trace.Component, "meta"),
	}, nil
}

// Update updates cloud metadata of the provided database.
func (m *Metadata) Update(ctx context.Context, database types.Database) error {
	if database.IsRDS() {
		return m.updateAWS(ctx, database, m.fetchRDSMetadata)
	} else if database.IsRedshift() {
		return m.updateAWS(ctx, database, m.fetchRedshiftMetadata)
	} else if database.IsElastiCache() {
		return m.updateAWS(ctx, database, m.fetchElastiCacheMetadata)
	}
	return nil
}

// updateAWS updates cloud metadata of the provided AWS database.
func (m *Metadata) updateAWS(ctx context.Context, database types.Database, fetchFn func(context.Context, types.Database) (*types.AWS, error)) error {
	metadata, err := fetchFn(ctx, database)
	if err != nil {
		if trace.IsAccessDenied(err) { // Permission errors are expected.
			m.log.WithError(err).Debugf("No permissions to fetch metadata for %q.", database)
			return nil
		}
		return trace.Wrap(err)
	}

	m.log.Debugf("Fetched metadata for %q: %v.", database, metadata)
	database.SetStatusAWS(*metadata)
	return nil
}

// fetchRDSMetadata fetches metadata for the provided RDS or Aurora database.
func (m *Metadata) fetchRDSMetadata(ctx context.Context, database types.Database) (*types.AWS, error) {
	rds, err := m.cfg.Clients.GetAWSRDSClient(database.GetAWS().Region)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// First try to fetch the RDS instance metadata.
	metadata, err := fetchRDSInstanceMetadata(ctx, rds, database.GetAWS().RDS.InstanceID)
	if err != nil && !trace.IsNotFound(err) && !trace.IsAccessDenied(err) {
		return nil, trace.Wrap(err)
	}
	// If RDS instance metadata wasn't found, it may be an Aurora cluster.
	if metadata == nil {
		// Aurora cluster ID may be either explicitly specified or parsed
		// from endpoint in which case it will be in InstanceID field.
		clusterID := database.GetAWS().RDS.ClusterID
		if clusterID == "" {
			clusterID = database.GetAWS().RDS.InstanceID
		}
		return fetchRDSClusterMetadata(ctx, rds, clusterID)
	}
	// If instance was found, it may be a part of an Aurora cluster.
	if metadata.RDS.ClusterID != "" {
		return fetchRDSClusterMetadata(ctx, rds, metadata.RDS.ClusterID)
	}
	return metadata, nil
}

// fetchRedshiftMetadata fetches metadata for the provided Redshift database.
func (m *Metadata) fetchRedshiftMetadata(ctx context.Context, database types.Database) (*types.AWS, error) {
	redshift, err := m.cfg.Clients.GetAWSRedshiftClient(database.GetAWS().Region)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cluster, err := describeRedshiftCluster(ctx, redshift, database.GetAWS().Redshift.ClusterID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	parsedARN, err := arn.Parse(aws.StringValue(cluster.ClusterNamespaceArn))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &types.AWS{
		Region:    parsedARN.Region,
		AccountID: parsedARN.AccountID,
		Redshift: types.Redshift{
			ClusterID: aws.StringValue(cluster.ClusterIdentifier),
		},
	}, nil
}

// fetchElastiCacheMetadata fetches metadata for the provided ElastiCache database.
func (m *Metadata) fetchElastiCacheMetadata(ctx context.Context, database types.Database) (*types.AWS, error) {
	elastiCacheClient, err := m.cfg.Clients.GetAWSElastiCacheClient(database.GetAWS().Region)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cluster, err := describeElastiCacheCluster(ctx, elastiCacheClient, database.GetAWS().ElastiCache.ReplicationGroupID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Endpoint type does not change.
	endpointType := database.GetAWS().ElastiCache.EndpointType
	return services.MetadataFromElastiCacheCluster(cluster, endpointType)
}

// fetchRDSInstanceMetadata fetches metadata about specified RDS instance.
func fetchRDSInstanceMetadata(ctx context.Context, rdsClient rdsiface.RDSAPI, instanceID string) (*types.AWS, error) {
	rdsInstance, err := describeRDSInstance(ctx, rdsClient, instanceID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return services.MetadataFromRDSInstance(rdsInstance)
}

// describeRDSInstance returns AWS RDS instance for the specified ID.
func describeRDSInstance(ctx context.Context, rdsClient rdsiface.RDSAPI, instanceID string) (*rds.DBInstance, error) {
	out, err := rdsClient.DescribeDBInstancesWithContext(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(instanceID),
	})
	if err != nil {
		return nil, common.ConvertError(err)
	}
	if len(out.DBInstances) != 1 {
		return nil, trace.BadParameter("expected 1 RDS instance for %v, got %+v", instanceID, out.DBInstances)
	}
	return out.DBInstances[0], nil
}

// fetchRDSClusterMetadata fetches metadata about specified Aurora cluster.
func fetchRDSClusterMetadata(ctx context.Context, rdsClient rdsiface.RDSAPI, clusterID string) (*types.AWS, error) {
	rdsCluster, err := describeRDSCluster(ctx, rdsClient, clusterID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return services.MetadataFromRDSCluster(rdsCluster)
}

// describeRDSCluster returns AWS Aurora cluster for the specified ID.
func describeRDSCluster(ctx context.Context, rdsClient rdsiface.RDSAPI, clusterID string) (*rds.DBCluster, error) {
	out, err := rdsClient.DescribeDBClustersWithContext(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(clusterID),
	})
	if err != nil {
		return nil, common.ConvertError(err)
	}
	if len(out.DBClusters) != 1 {
		return nil, trace.BadParameter("expected 1 RDS cluster for %v, got %+v", clusterID, out.DBClusters)
	}
	return out.DBClusters[0], nil
}

// describeRedshiftCluster returns AWS Redshift cluster for the specified ID.
func describeRedshiftCluster(ctx context.Context, redshiftClient redshiftiface.RedshiftAPI, clusterID string) (*redshift.Cluster, error) {
	out, err := redshiftClient.DescribeClustersWithContext(ctx, &redshift.DescribeClustersInput{
		ClusterIdentifier: aws.String(clusterID),
	})
	if err != nil {
		return nil, common.ConvertError(err)
	}
	if len(out.Clusters) != 1 {
		return nil, trace.BadParameter("expected 1 Redshift cluster for %v, got %+v", clusterID, out.Clusters)
	}
	return out.Clusters[0], nil
}

// describeElastiCacheCluster returns AWS ElastiCache Redis cluster for the
// specified ID.
func describeElastiCacheCluster(ctx context.Context, elastiCacheClient elasticacheiface.ElastiCacheAPI, replicationGroupID string) (*elasticache.ReplicationGroup, error) {
	out, err := elastiCacheClient.DescribeReplicationGroupsWithContext(ctx, &elasticache.DescribeReplicationGroupsInput{
		ReplicationGroupId: aws.String(replicationGroupID),
	})
	if err != nil {
		return nil, common.ConvertError(err)
	}
	if len(out.ReplicationGroups) != 1 {
		return nil, trace.BadParameter("expected 1 ElastiCache cluster for %v, got %+v", replicationGroupID, out.ReplicationGroups)
	}
	return out.ReplicationGroups[0], nil
}
