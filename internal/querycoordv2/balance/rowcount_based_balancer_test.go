// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package balance

import (
	"testing"

	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	. "github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/internal/util/etcd"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

type RowCountBasedBalancerTestSuite struct {
	suite.Suite
	balancer *RowCountBasedBalancer
	kv       *etcdkv.EtcdKV
	broker   *meta.MockBroker
}

func (suite *RowCountBasedBalancerTestSuite) SetupSuite() {
	Params.Init()
}

func (suite *RowCountBasedBalancerTestSuite) SetupTest() {
	var err error
	config := GenerateEtcdConfig()
	cli, err := etcd.GetEtcdClient(
		config.UseEmbedEtcd.GetAsBool(),
		config.EtcdUseSSL.GetAsBool(),
		config.Endpoints.GetAsStrings(),
		config.EtcdTLSCert.GetValue(),
		config.EtcdTLSKey.GetValue(),
		config.EtcdTLSCACert.GetValue(),
		config.EtcdTLSMinVersion.GetValue())
	suite.Require().NoError(err)
	suite.kv = etcdkv.NewEtcdKV(cli, config.MetaRootPath.GetValue())
	suite.broker = meta.NewMockBroker(suite.T())

	store := meta.NewMetaStore(suite.kv)
	idAllocator := RandomIncrementIDAllocator()
	testMeta := meta.NewMeta(idAllocator, store)
	testTarget := meta.NewTargetManager(suite.broker, testMeta)

	distManager := meta.NewDistributionManager()
	nodeManager := session.NewNodeManager()
	suite.balancer = NewRowCountBasedBalancer(nil, nodeManager, distManager, testMeta, testTarget)
}

func (suite *RowCountBasedBalancerTestSuite) TearDownTest() {
	suite.kv.Close()
}

func (suite *RowCountBasedBalancerTestSuite) TestAssignSegment() {
	cases := []struct {
		name          string
		distributions map[int64][]*meta.Segment
		assignments   []*meta.Segment
		nodes         []int64
		expectPlans   []SegmentAssignPlan
	}{
		{
			name: "test normal assignment",
			distributions: map[int64][]*meta.Segment{
				2: {{SegmentInfo: &datapb.SegmentInfo{ID: 1, NumOfRows: 20}, Node: 2}},
				3: {{SegmentInfo: &datapb.SegmentInfo{ID: 2, NumOfRows: 30}, Node: 3}},
			},
			assignments: []*meta.Segment{
				{SegmentInfo: &datapb.SegmentInfo{ID: 3, NumOfRows: 5}},
				{SegmentInfo: &datapb.SegmentInfo{ID: 4, NumOfRows: 10}},
				{SegmentInfo: &datapb.SegmentInfo{ID: 5, NumOfRows: 15}},
			},
			nodes: []int64{1, 2, 3},
			expectPlans: []SegmentAssignPlan{
				{Segment: &meta.Segment{SegmentInfo: &datapb.SegmentInfo{ID: 3, NumOfRows: 5}}, From: -1, To: 2},
				{Segment: &meta.Segment{SegmentInfo: &datapb.SegmentInfo{ID: 4, NumOfRows: 10}}, From: -1, To: 1},
				{Segment: &meta.Segment{SegmentInfo: &datapb.SegmentInfo{ID: 5, NumOfRows: 15}}, From: -1, To: 1},
			},
		},
		// TODO: add more cases
	}

	for _, c := range cases {
		suite.Run(c.name, func() {
			// I do not find a better way to do the setup and teardown work for subtests yet.
			// If you do, please replace with it.
			suite.SetupSuite()
			defer suite.TearDownTest()
			balancer := suite.balancer
			for node, s := range c.distributions {
				balancer.dist.SegmentDistManager.Update(node, s...)
			}
			plans := balancer.AssignSegment(c.assignments, c.nodes)
			suite.ElementsMatch(c.expectPlans, plans)
		})
	}
}

func (suite *RowCountBasedBalancerTestSuite) TestBalance() {
	cases := []struct {
		name          string
		nodes         []int64
		distributions map[int64][]*meta.Segment
		expectPlans   []SegmentAssignPlan
	}{
		{
			name:  "normal balance",
			nodes: []int64{1, 2},
			distributions: map[int64][]*meta.Segment{
				1: {{SegmentInfo: &datapb.SegmentInfo{ID: 1, CollectionID: 1, NumOfRows: 10}, Node: 1}},
				2: {
					{SegmentInfo: &datapb.SegmentInfo{ID: 2, CollectionID: 1, NumOfRows: 20}, Node: 2},
					{SegmentInfo: &datapb.SegmentInfo{ID: 3, CollectionID: 1, NumOfRows: 30}, Node: 2},
				},
			},
			expectPlans: []SegmentAssignPlan{
				{Segment: &meta.Segment{SegmentInfo: &datapb.SegmentInfo{ID: 2, CollectionID: 1, NumOfRows: 20}, Node: 2}, From: 2, To: 1, ReplicaID: 1},
			},
		},
		{
			name:  "already balanced",
			nodes: []int64{1, 2},
			distributions: map[int64][]*meta.Segment{
				1: {{SegmentInfo: &datapb.SegmentInfo{ID: 1, CollectionID: 1, NumOfRows: 30}, Node: 1}},
				2: {
					{SegmentInfo: &datapb.SegmentInfo{ID: 2, CollectionID: 1, NumOfRows: 20}, Node: 2},
					{SegmentInfo: &datapb.SegmentInfo{ID: 3, CollectionID: 1, NumOfRows: 30}, Node: 2},
				},
			},
			expectPlans: []SegmentAssignPlan{},
		},
	}

	for _, c := range cases {
		suite.Run(c.name, func() {
			suite.SetupSuite()
			defer suite.TearDownTest()
			balancer := suite.balancer
			collection := utils.CreateTestCollection(1, 1)
			segments := []*datapb.SegmentBinlogs{
				{
					SegmentID: 1,
				},
				{
					SegmentID: 2,
				},
				{
					SegmentID: 3,
				},
			}
			suite.broker.EXPECT().GetRecoveryInfo(mock.Anything, int64(1), int64(1)).Return(
				nil, segments, nil)
			balancer.targetMgr.UpdateCollectionNextTargetWithPartitions(int64(1), int64(1))
			balancer.targetMgr.UpdateCollectionCurrentTarget(1, 1)
			collection.LoadPercentage = 100
			collection.Status = querypb.LoadStatus_Loaded
			balancer.meta.CollectionManager.PutCollection(collection)
			balancer.meta.ReplicaManager.Put(utils.CreateTestReplica(1, 1, c.nodes))
			for node, s := range c.distributions {
				balancer.dist.SegmentDistManager.Update(node, s...)
			}
			segmentPlans, channelPlans := balancer.Balance()
			suite.Empty(channelPlans)
			suite.ElementsMatch(c.expectPlans, segmentPlans)
		})
	}

}

func (suite *RowCountBasedBalancerTestSuite) TestBalanceOnLoadingCollection() {
	cases := []struct {
		name          string
		nodes         []int64
		distributions map[int64][]*meta.Segment
		expectPlans   []SegmentAssignPlan
	}{
		{
			name:  "normal balance",
			nodes: []int64{1, 2},
			distributions: map[int64][]*meta.Segment{
				1: {{SegmentInfo: &datapb.SegmentInfo{ID: 1, CollectionID: 1, NumOfRows: 10}, Node: 1}},
				2: {
					{SegmentInfo: &datapb.SegmentInfo{ID: 2, CollectionID: 1, NumOfRows: 20}, Node: 2},
					{SegmentInfo: &datapb.SegmentInfo{ID: 3, CollectionID: 1, NumOfRows: 30}, Node: 2},
				},
			},
			expectPlans: []SegmentAssignPlan{},
		},
	}

	for _, c := range cases {
		suite.Run(c.name, func() {
			suite.SetupSuite()
			defer suite.TearDownTest()
			balancer := suite.balancer
			collection := utils.CreateTestCollection(1, 1)
			collection.LoadPercentage = 100
			collection.Status = querypb.LoadStatus_Loading
			balancer.meta.CollectionManager.PutCollection(collection)
			balancer.meta.ReplicaManager.Put(utils.CreateTestReplica(1, 1, c.nodes))
			for node, s := range c.distributions {
				balancer.dist.SegmentDistManager.Update(node, s...)
			}
			segmentPlans, channelPlans := balancer.Balance()
			suite.Empty(channelPlans)
			suite.ElementsMatch(c.expectPlans, segmentPlans)
		})
	}

}

func TestRowCountBasedBalancerSuite(t *testing.T) {
	suite.Run(t, new(RowCountBasedBalancerTestSuite))
}
