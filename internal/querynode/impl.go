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

package querynode

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/metrics"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/timerecord"
	"github.com/milvus-io/milvus/internal/util/trace"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

// GetComponentStates returns information about whether the node is healthy
func (node *QueryNode) GetComponentStates(ctx context.Context) (*milvuspb.ComponentStates, error) {
	stats := &milvuspb.ComponentStates{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
	}
	code, ok := node.stateCode.Load().(commonpb.StateCode)
	if !ok {
		errMsg := "unexpected error in type assertion"
		stats.Status = &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    errMsg,
		}
		return stats, nil
	}
	nodeID := common.NotRegisteredID
	if node.session != nil && node.session.Registered() {
		nodeID = paramtable.GetNodeID()
	}
	info := &milvuspb.ComponentInfo{
		NodeID:    nodeID,
		Role:      typeutil.QueryNodeRole,
		StateCode: code,
	}
	stats.State = info
	log.Debug("Get QueryNode component state done", zap.Any("stateCode", info.StateCode))
	return stats, nil
}

// GetTimeTickChannel returns the time tick channel
// TimeTickChannel contains many time tick messages, which will be sent by query nodes
func (node *QueryNode) GetTimeTickChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
		Value: Params.CommonCfg.QueryCoordTimeTick,
	}, nil
}

// GetStatisticsChannel returns the statistics channel
// Statistics channel contains statistics infos of query nodes, such as segment infos, memory infos
func (node *QueryNode) GetStatisticsChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
	}, nil
}

func (node *QueryNode) GetStatistics(ctx context.Context, req *querypb.GetStatisticsRequest) (*internalpb.GetStatisticsResponse, error) {
	log.Ctx(ctx).Debug("received GetStatisticsRequest",
		zap.Strings("vChannels", req.GetDmlChannels()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.GetReq().GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	failRet := &internalpb.GetStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	toReduceResults := make([]*internalpb.GetStatisticsResponse, 0)
	runningGp, runningCtx := errgroup.WithContext(ctx)
	mu := &sync.Mutex{}
	for _, ch := range req.GetDmlChannels() {
		ch := ch
		req := &querypb.GetStatisticsRequest{
			Req:             req.Req,
			DmlChannels:     []string{ch},
			SegmentIDs:      req.SegmentIDs,
			FromShardLeader: req.FromShardLeader,
			Scope:           req.Scope,
		}
		runningGp.Go(func() error {
			ret, err := node.getStatisticsWithDmlChannel(runningCtx, req, ch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failRet.Status.Reason = err.Error()
				failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
				return err
			}
			if ret.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
				failRet.Status.Reason = ret.Status.Reason
				failRet.Status.ErrorCode = ret.Status.ErrorCode
				return fmt.Errorf("%s", ret.Status.Reason)
			}
			toReduceResults = append(toReduceResults, ret)
			return nil
		})
	}
	if err := runningGp.Wait(); err != nil {
		return failRet, nil
	}
	ret, err := reduceStatisticResponse(toReduceResults)
	if err != nil {
		failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}
	return ret, nil
}

func (node *QueryNode) getStatisticsWithDmlChannel(ctx context.Context, req *querypb.GetStatisticsRequest, dmlChannel string) (*internalpb.GetStatisticsResponse, error) {
	failRet := &internalpb.GetStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	if !node.isHealthy() {
		failRet.Status.Reason = msgQueryNodeIsUnhealthy(paramtable.GetNodeID())
		return failRet, nil
	}

	traceID, _, _ := trace.InfoFromContext(ctx)
	log.Ctx(ctx).Debug("received GetStatisticRequest",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.GetReq().GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	if node.queryShardService == nil {
		failRet.Status.Reason = "queryShardService is nil"
		return failRet, nil
	}

	qs, err := node.queryShardService.getQueryShard(dmlChannel)
	if err != nil {
		log.Warn("get statistics failed, failed to get query shard",
			zap.String("dml channel", dmlChannel),
			zap.Error(err))
		failRet.Status.ErrorCode = commonpb.ErrorCode_NotShardLeader
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}

	log.Debug("start do statistics",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()))
	tr := timerecord.NewTimeRecorder("")

	waitCanDo := func(ctx context.Context) error {
		l := node.tSafeReplica.WatchChannel(dmlChannel)
		defer l.Unregister()
		for {
			select {
			case <-l.On():
				serviceTime, err := qs.getServiceableTime(dmlChannel)
				if err != nil {
					return err
				}
				guaranteeTs := req.GetReq().GetGuaranteeTimestamp()
				if guaranteeTs <= serviceTime {
					return nil
				}
			case <-ctx.Done():
				return errors.New("get statistics context timeout")
			}
		}
	}

	if req.FromShardLeader {
		historicalTask := newStatistics(ctx, req, querypb.DataScope_Historical, qs, waitCanDo)
		err := historicalTask.Execute(ctx)
		if err != nil {
			failRet.Status.Reason = err.Error()
			return failRet, nil
		}

		tr.Elapse(fmt.Sprintf("do statistics done, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
			traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))
		failRet.Status.ErrorCode = commonpb.ErrorCode_Success
		return historicalTask.Ret, nil
	}

	// from Proxy

	cluster, ok := qs.clusterService.getShardCluster(dmlChannel)
	if !ok {
		failRet.Status.ErrorCode = commonpb.ErrorCode_NotShardLeader
		failRet.Status.Reason = fmt.Sprintf("channel %s leader is not here", dmlChannel)
		return failRet, nil
	}

	statisticCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var results []*internalpb.GetStatisticsResponse
	var streamingResult *internalpb.GetStatisticsResponse
	var errCluster error

	withStreaming := func(ctx context.Context) error {
		streamingTask := newStatistics(ctx, req, querypb.DataScope_Streaming, qs, waitCanDo)
		err := streamingTask.Execute(ctx)
		if err != nil {
			return err
		}
		streamingResult = streamingTask.Ret
		return nil
	}

	// shard leader dispatches request to its shard cluster
	results, errCluster = cluster.GetStatistics(statisticCtx, req, withStreaming)

	if errCluster != nil {
		log.Warn("get statistics on cluster failed",
			zap.Int64("collectionID", req.Req.GetCollectionID()),
			zap.Error(errCluster))
		failRet.Status.Reason = errCluster.Error()
		return failRet, nil
	}

	tr.Elapse(fmt.Sprintf("start reduce statistic result, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	results = append(results, streamingResult)
	ret, err := reduceStatisticResponse(results)
	if err != nil {
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}
	log.Debug("reduce statistic result done",
		zap.Any("results", ret))

	tr.Elapse(fmt.Sprintf("do statistics done, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	failRet.Status.ErrorCode = commonpb.ErrorCode_Success
	return ret, nil
}

// WatchDmChannels create consumers on dmChannels to receive Incremental data，which is the important part of real-time query
func (node *QueryNode) WatchDmChannels(ctx context.Context, in *querypb.WatchDmChannelsRequest) (*commonpb.Status, error) {
	// check node healthy
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}

	// check target matches
	if in.GetBase().GetTargetID() != paramtable.GetNodeID() {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(in.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return status, nil
	}

	log := log.With(
		zap.Int64("collectionID", in.GetCollectionID()),
		zap.Int64("nodeID", paramtable.GetNodeID()),
		zap.Strings("channels", lo.Map(in.GetInfos(), func(info *datapb.VchannelInfo, _ int) string {
			return info.GetChannelName()
		})),
	)

	task := &watchDmChannelsTask{
		baseTask: baseTask{
			ctx:  ctx,
			done: make(chan error),
		},
		req:  in,
		node: node,
	}

	startTs := time.Now()
	log.Info("watchDmChannels init")
	// currently we only support load one channel as a time
	future := node.taskPool.Submit(func() (interface{}, error) {
		log.Info("watchDmChannels start ",
			zap.Duration("timeInQueue", time.Since(startTs)))
		err := task.PreExecute(ctx)
		if err != nil {
			status := &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			}
			log.Warn("failed to subscribe channel on preExecute ", zap.Error(err))
			return status, nil
		}

		err = task.Execute(ctx)
		if err != nil {
			status := &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			}
			log.Warn("failed to subscribe channel", zap.Error(err))
			return status, nil
		}

		err = task.PostExecute(ctx)
		if err != nil {
			status := &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			}
			log.Warn("failed to unsubscribe channel on postExecute ", zap.Error(err))
			return status, nil
		}

		log.Info("successfully watchDmChannelsTask")
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		}, nil
	})
	ret, _ := future.Await()
	return ret.(*commonpb.Status), nil
}

func (node *QueryNode) UnsubDmChannel(ctx context.Context, req *querypb.UnsubDmChannelRequest) (*commonpb.Status, error) {
	// check node healthy
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}

	// check target matches
	if req.GetBase().GetTargetID() != paramtable.GetNodeID() {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(req.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return status, nil
	}

	dct := &releaseCollectionTask{
		baseTask: baseTask{
			ctx:  ctx,
			done: make(chan error),
		},
		req: &querypb.ReleaseCollectionRequest{
			Base:         req.GetBase(),
			CollectionID: req.GetCollectionID(),
			NodeID:       req.GetNodeID(),
		},
		node: node,
	}

	err := node.scheduler.queue.Enqueue(dct)
	if err != nil {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		log.Warn("failed to enqueue subscribe channel task", zap.Error(err))
		return status, nil
	}
	log.Info("unsubDmChannel(ReleaseCollection) enqueue done", zap.Int64("collectionID", req.GetCollectionID()))

	func() {
		err = dct.WaitToFinish()
		if err != nil {
			log.Warn("failed to do subscribe channel task successfully", zap.Error(err))
			return
		}
		log.Info("unsubDmChannel(ReleaseCollection) WaitToFinish done", zap.Int64("collectionID", req.GetCollectionID()))
	}()

	status := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}
	return status, nil
}

// LoadSegments load historical data into query node, historical data can be vector data or index
func (node *QueryNode) LoadSegments(ctx context.Context, in *querypb.LoadSegmentsRequest) (*commonpb.Status, error) {
	// check node healthy
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}
	// check target matches
	if in.GetBase().GetTargetID() != paramtable.GetNodeID() {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(in.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return status, nil
	}

	if in.GetNeedTransfer() {
		return node.TransferLoad(ctx, in)
	}

	task := &loadSegmentsTask{
		baseTask: baseTask{
			ctx:  ctx,
			done: make(chan error),
		},
		req:  in,
		node: node,
	}

	segmentIDs := make([]UniqueID, 0, len(in.GetInfos()))
	for _, info := range in.Infos {
		segmentIDs = append(segmentIDs, info.SegmentID)
	}
	sort.SliceStable(segmentIDs, func(i, j int) bool {
		return segmentIDs[i] < segmentIDs[j]
	})

	startTs := time.Now()
	log.Info("loadSegmentsTask init", zap.Int64("collectionID", in.CollectionID),
		zap.Int64s("segmentIDs", segmentIDs),
		zap.Int64("nodeID", paramtable.GetNodeID()))

	// TODO remove concurrent load segment for now, unless we solve the memory issue
	log.Info("loadSegmentsTask start ", zap.Int64("collectionID", in.CollectionID),
		zap.Int64s("segmentIDs", segmentIDs),
		zap.Duration("timeInQueue", time.Since(startTs)))
	err := node.scheduler.queue.Enqueue(task)
	if err != nil {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		log.Warn(err.Error())
		return status, nil
	}

	log.Info("loadSegmentsTask Enqueue done", zap.Int64("collectionID", in.CollectionID), zap.Int64s("segmentIDs", segmentIDs), zap.Int64("nodeID", paramtable.GetNodeID()))

	waitFunc := func() (*commonpb.Status, error) {
		err = task.WaitToFinish()
		if err != nil {
			status := &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			}
			log.Warn(err.Error())
			return status, nil
		}
		log.Info("loadSegmentsTask WaitToFinish done", zap.Int64("collectionID", in.CollectionID), zap.Int64s("segmentIDs", segmentIDs), zap.Int64("nodeID", paramtable.GetNodeID()))
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		}, nil
	}

	return waitFunc()
}

// ReleaseCollection clears all data related to this collection on the querynode
func (node *QueryNode) ReleaseCollection(ctx context.Context, in *querypb.ReleaseCollectionRequest) (*commonpb.Status, error) {
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}
	dct := &releaseCollectionTask{
		baseTask: baseTask{
			ctx:  ctx,
			done: make(chan error),
		},
		req:  in,
		node: node,
	}

	err := node.scheduler.queue.Enqueue(dct)
	if err != nil {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		log.Warn(err.Error())
		return status, nil
	}
	log.Info("releaseCollectionTask Enqueue done", zap.Int64("collectionID", in.CollectionID))

	func() {
		err = dct.WaitToFinish()
		if err != nil {
			log.Warn(err.Error())
			return
		}
		log.Info("releaseCollectionTask WaitToFinish done", zap.Int64("collectionID", in.CollectionID))
	}()

	status := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}
	return status, nil
}

// ReleasePartitions clears all data related to this partition on the querynode
func (node *QueryNode) ReleasePartitions(ctx context.Context, in *querypb.ReleasePartitionsRequest) (*commonpb.Status, error) {
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}
	dct := &releasePartitionsTask{
		baseTask: baseTask{
			ctx:  ctx,
			done: make(chan error),
		},
		req:  in,
		node: node,
	}

	err := node.scheduler.queue.Enqueue(dct)
	if err != nil {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		log.Warn(err.Error())
		return status, nil
	}
	log.Info("releasePartitionsTask Enqueue done", zap.Int64("collectionID", in.CollectionID), zap.Int64s("partitionIDs", in.PartitionIDs))

	func() {
		err = dct.WaitToFinish()
		if err != nil {
			log.Warn(err.Error())
			return
		}
		log.Info("releasePartitionsTask WaitToFinish done", zap.Int64("collectionID", in.CollectionID), zap.Int64s("partitionIDs", in.PartitionIDs))
	}()

	status := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}
	return status, nil
}

// ReleaseSegments remove the specified segments from query node according segmentIDs, partitionIDs, and collectionID
func (node *QueryNode) ReleaseSegments(ctx context.Context, in *querypb.ReleaseSegmentsRequest) (*commonpb.Status, error) {
	// check node healthy
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}
	// check target matches
	if in.GetBase().GetTargetID() != paramtable.GetNodeID() {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(in.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return status, nil
	}

	if in.GetNeedTransfer() {
		return node.TransferRelease(ctx, in)
	}

	log.Info("start to release segments", zap.Int64("collectionID", in.CollectionID), zap.Int64s("segmentIDs", in.SegmentIDs))

	for _, id := range in.SegmentIDs {
		switch in.GetScope() {
		case querypb.DataScope_Streaming:
			node.metaReplica.removeSegment(id, segmentTypeGrowing)
		case querypb.DataScope_Historical:
			node.metaReplica.removeSegment(id, segmentTypeSealed)
		case querypb.DataScope_All:
			node.metaReplica.removeSegment(id, segmentTypeSealed)
			node.metaReplica.removeSegment(id, segmentTypeGrowing)
		}
	}

	// note that argument is dmlchannel name
	node.dataSyncService.removeEmptyFlowGraphByChannel(in.GetCollectionID(), in.GetShard())

	log.Info("release segments done", zap.Int64("collectionID", in.CollectionID), zap.Int64s("segmentIDs", in.SegmentIDs), zap.String("Scope", in.GetScope().String()))
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

// GetSegmentInfo returns segment information of the collection on the queryNode, and the information includes memSize, numRow, indexName, indexID ...
func (node *QueryNode) GetSegmentInfo(ctx context.Context, in *querypb.GetSegmentInfoRequest) (*querypb.GetSegmentInfoResponse, error) {
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		res := &querypb.GetSegmentInfoResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			},
		}
		return res, nil
	}
	var segmentInfos []*querypb.SegmentInfo

	segmentIDs := make(map[int64]struct{})
	for _, segmentID := range in.GetSegmentIDs() {
		segmentIDs[segmentID] = struct{}{}
	}

	infos := node.metaReplica.getSegmentInfosByColID(in.CollectionID)
	segmentInfos = append(segmentInfos, filterSegmentInfo(infos, segmentIDs)...)

	return &querypb.GetSegmentInfoResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Infos: segmentInfos,
	}, nil
}

// filterSegmentInfo returns segment info which segment id in segmentIDs map
func filterSegmentInfo(segmentInfos []*querypb.SegmentInfo, segmentIDs map[int64]struct{}) []*querypb.SegmentInfo {
	if len(segmentIDs) == 0 {
		return segmentInfos
	}
	filtered := make([]*querypb.SegmentInfo, 0, len(segmentIDs))
	for _, info := range segmentInfos {
		_, ok := segmentIDs[info.GetSegmentID()]
		if !ok {
			continue
		}
		filtered = append(filtered, info)
	}
	return filtered
}

// isHealthy checks if QueryNode is healthy
func (node *QueryNode) isHealthy() bool {
	code := node.stateCode.Load().(commonpb.StateCode)
	return code == commonpb.StateCode_Healthy
}

// Search performs replica search tasks.
func (node *QueryNode) Search(ctx context.Context, req *querypb.SearchRequest) (*internalpb.SearchResults, error) {
	log.Ctx(ctx).Debug("Received SearchRequest",
		zap.Strings("vChannels", req.GetDmlChannels()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.GetReq().GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	if req.GetReq().GetBase().GetTargetID() != paramtable.GetNodeID() {
		return &internalpb.SearchResults{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
				Reason:    common.WrapNodeIDNotMatchMsg(req.GetReq().GetBase().GetTargetID(), paramtable.GetNodeID()),
			},
		}, nil
	}

	failRet := &internalpb.SearchResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
	}
	toReduceResults := make([]*internalpb.SearchResults, 0)
	runningGp, runningCtx := errgroup.WithContext(ctx)
	mu := &sync.Mutex{}
	for _, ch := range req.GetDmlChannels() {
		ch := ch
		req := &querypb.SearchRequest{
			Req:             req.Req,
			DmlChannels:     []string{ch},
			SegmentIDs:      req.SegmentIDs,
			FromShardLeader: req.FromShardLeader,
			Scope:           req.Scope,
		}
		runningGp.Go(func() error {
			ret, err := node.searchWithDmlChannel(runningCtx, req, ch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failRet.Status.Reason = err.Error()
				failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
				return err
			}
			if ret.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
				failRet.Status.Reason = ret.Status.Reason
				failRet.Status.ErrorCode = ret.Status.ErrorCode
				return fmt.Errorf("%s", ret.Status.Reason)
			}
			toReduceResults = append(toReduceResults, ret)
			return nil
		})
	}
	if err := runningGp.Wait(); err != nil {
		return failRet, nil
	}
	ret, err := reduceSearchResults(ctx, toReduceResults, req.Req.GetNq(), req.Req.GetTopk(), req.Req.GetMetricType())
	if err != nil {
		failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}

	if !req.FromShardLeader {
		rateCol.Add(metricsinfo.NQPerSecond, float64(req.GetReq().GetNq()))
		rateCol.Add(metricsinfo.SearchThroughput, float64(proto.Size(req)))
		metrics.QueryNodeExecuteCounter.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), metrics.SearchLabel).Add(float64(proto.Size(req)))
	}
	return ret, nil
}

func (node *QueryNode) searchWithDmlChannel(ctx context.Context, req *querypb.SearchRequest, dmlChannel string) (*internalpb.SearchResults, error) {
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.TotalLabel).Inc()
	failRet := &internalpb.SearchResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	defer func() {
		if failRet.Status.ErrorCode != commonpb.ErrorCode_Success {
			metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.FailLabel).Inc()
		}
	}()
	if !node.isHealthy() {
		failRet.Status.Reason = msgQueryNodeIsUnhealthy(paramtable.GetNodeID())
		return failRet, nil
	}

	msgID := req.GetReq().GetBase().GetMsgID()
	log.Ctx(ctx).Debug("Received SearchRequest",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.GetReq().GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	if node.queryShardService == nil {
		failRet.Status.Reason = "queryShardService is nil"
		return failRet, nil
	}

	qs, err := node.queryShardService.getQueryShard(dmlChannel)
	if err != nil {
		log.Ctx(ctx).Warn("Search failed, failed to get query shard",
			zap.String("dml channel", dmlChannel),
			zap.Error(err))
		failRet.Status.ErrorCode = commonpb.ErrorCode_NotShardLeader
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}

	log.Ctx(ctx).Debug("start do search",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()))
	tr := timerecord.NewTimeRecorder("")

	if req.FromShardLeader {
		historicalTask, err2 := newSearchTask(ctx, req)
		if err2 != nil {
			failRet.Status.Reason = err2.Error()
			return failRet, nil
		}
		historicalTask.QS = qs
		historicalTask.DataScope = querypb.DataScope_Historical
		err2 = node.scheduler.AddReadTask(ctx, historicalTask)
		if err2 != nil {
			failRet.Status.Reason = err2.Error()
			return failRet, nil
		}

		err2 = historicalTask.WaitToFinish()
		if err2 != nil {
			failRet.Status.Reason = err2.Error()
			return failRet, nil
		}

		tr.CtxElapse(ctx, fmt.Sprintf("do search done, msgID = %d, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
			msgID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

		failRet.Status.ErrorCode = commonpb.ErrorCode_Success
		metrics.QueryNodeSQLatencyInQueue.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.SearchLabel).Observe(float64(historicalTask.queueDur.Milliseconds()))
		metrics.QueryNodeReduceLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.SearchLabel).Observe(float64(historicalTask.reduceDur.Milliseconds()))
		latency := tr.ElapseSpan()
		metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.FromLeader).Observe(float64(latency.Milliseconds()))
		metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.SuccessLabel).Inc()
		return historicalTask.Ret, nil
	}

	//from Proxy
	cluster, ok := qs.clusterService.getShardCluster(dmlChannel)
	if !ok {
		failRet.Status.ErrorCode = commonpb.ErrorCode_NotShardLeader
		failRet.Status.Reason = fmt.Sprintf("channel %s leader is not here", dmlChannel)
		return failRet, nil
	}

	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var results []*internalpb.SearchResults
	var streamingResult *internalpb.SearchResults
	var errCluster error

	withStreaming := func(ctx context.Context) error {
		streamingTask, err := newSearchTask(searchCtx, req)
		if err != nil {
			return err
		}
		streamingTask.QS = qs
		streamingTask.DataScope = querypb.DataScope_Streaming
		err = node.scheduler.AddReadTask(searchCtx, streamingTask)
		if err != nil {
			return err
		}
		err = streamingTask.WaitToFinish()
		if err != nil {
			return err
		}
		metrics.QueryNodeSQLatencyInQueue.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.SearchLabel).Observe(float64(streamingTask.queueDur.Milliseconds()))
		metrics.QueryNodeReduceLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.SearchLabel).Observe(float64(streamingTask.reduceDur.Milliseconds()))
		streamingResult = streamingTask.Ret
		return nil
	}

	// shard leader dispatches request to its shard cluster
	results, errCluster = cluster.Search(searchCtx, req, withStreaming)
	if errCluster != nil {
		log.Ctx(ctx).Warn("search cluster failed", zap.Int64("collectionID", req.Req.GetCollectionID()), zap.Error(errCluster))
		failRet.Status.Reason = errCluster.Error()
		return failRet, nil
	}

	tr.CtxElapse(ctx, fmt.Sprintf("start reduce search result, msgID = %d, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		msgID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	results = append(results, streamingResult)
	ret, err2 := reduceSearchResults(ctx, results, req.Req.GetNq(), req.Req.GetTopk(), req.Req.GetMetricType())
	if err2 != nil {
		failRet.Status.Reason = err2.Error()
		return failRet, nil
	}

	tr.CtxElapse(ctx, fmt.Sprintf("do search done, msgID = %d, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		msgID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	failRet.Status.ErrorCode = commonpb.ErrorCode_Success
	latency := tr.ElapseSpan()
	metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.Leader).Observe(float64(latency.Milliseconds()))
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.SuccessLabel).Inc()
	metrics.QueryNodeSearchNQ.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Observe(float64(req.Req.GetNq()))
	metrics.QueryNodeSearchTopK.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Observe(float64(req.Req.GetTopk()))

	return ret, nil
}

func (node *QueryNode) queryWithDmlChannel(ctx context.Context, req *querypb.QueryRequest, dmlChannel string) (*internalpb.RetrieveResults, error) {
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.TotalLabel).Inc()
	failRet := &internalpb.RetrieveResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	defer func() {
		if failRet.Status.ErrorCode != commonpb.ErrorCode_Success {
			metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.FailLabel).Inc()
		}
	}()
	if !node.isHealthy() {
		failRet.Status.Reason = msgQueryNodeIsUnhealthy(paramtable.GetNodeID())
		return failRet, nil
	}

	traceID, _, _ := trace.InfoFromContext(ctx)
	log.Ctx(ctx).Debug("queryWithDmlChannel receives query request",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.GetReq().GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	if node.queryShardService == nil {
		failRet.Status.Reason = "queryShardService is nil"
		return failRet, nil
	}

	qs, err := node.queryShardService.getQueryShard(dmlChannel)
	if err != nil {
		log.Ctx(ctx).Warn("Query failed, failed to get query shard",
			zap.String("dml channel", dmlChannel),
			zap.Error(err))
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}

	log.Ctx(ctx).Debug("queryWithDmlChannel starts do query",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.String("vChannel", dmlChannel),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()))
	tr := timerecord.NewTimeRecorder("")

	if req.FromShardLeader {
		// construct a queryTask
		queryTask := newQueryTask(ctx, req)
		queryTask.QS = qs
		queryTask.DataScope = querypb.DataScope_Historical
		err2 := node.scheduler.AddReadTask(ctx, queryTask)
		if err2 != nil {
			failRet.Status.Reason = err2.Error()
			return failRet, nil
		}

		err2 = queryTask.WaitToFinish()
		if err2 != nil {
			failRet.Status.Reason = err2.Error()
			return failRet, nil
		}

		tr.CtxElapse(ctx, fmt.Sprintf("do query done, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
			traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

		failRet.Status.ErrorCode = commonpb.ErrorCode_Success
		metrics.QueryNodeSQLatencyInQueue.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.QueryLabel).Observe(float64(queryTask.queueDur.Milliseconds()))
		metrics.QueryNodeReduceLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.QueryLabel).Observe(float64(queryTask.reduceDur.Milliseconds()))
		latency := tr.ElapseSpan()
		metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.FromLeader).Observe(float64(latency.Milliseconds()))
		metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.SuccessLabel).Inc()
		return queryTask.Ret, nil
	}

	cluster, ok := qs.clusterService.getShardCluster(dmlChannel)
	if !ok {
		failRet.Status.ErrorCode = commonpb.ErrorCode_NotShardLeader
		failRet.Status.Reason = fmt.Sprintf("channel %s leader is not here", dmlChannel)
		return failRet, nil
	}

	// add cancel when error occurs
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var results []*internalpb.RetrieveResults
	var streamingResult *internalpb.RetrieveResults

	withStreaming := func(ctx context.Context) error {
		streamingTask := newQueryTask(queryCtx, req)
		streamingTask.DataScope = querypb.DataScope_Streaming
		streamingTask.QS = qs
		err := node.scheduler.AddReadTask(queryCtx, streamingTask)

		if err != nil {
			return err
		}
		err = streamingTask.WaitToFinish()
		if err != nil {
			return err
		}
		metrics.QueryNodeSQLatencyInQueue.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.QueryLabel).Observe(float64(streamingTask.queueDur.Milliseconds()))
		metrics.QueryNodeReduceLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.QueryLabel).Observe(float64(streamingTask.reduceDur.Milliseconds()))
		streamingResult = streamingTask.Ret
		return nil
	}

	var errCluster error
	// shard leader dispatches request to its shard cluster
	results, errCluster = cluster.Query(queryCtx, req, withStreaming)
	if errCluster != nil {
		log.Ctx(ctx).Warn("failed to query cluster",
			zap.Int64("collectionID", req.Req.GetCollectionID()),
			zap.Error(errCluster))
		failRet.Status.Reason = errCluster.Error()
		return failRet, nil
	}

	tr.CtxElapse(ctx, fmt.Sprintf("start reduce query result, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	results = append(results, streamingResult)
	ret, err2 := mergeInternalRetrieveResult(ctx, results, req.Req.GetLimit())
	if err2 != nil {
		failRet.Status.Reason = err2.Error()
		return failRet, nil
	}

	tr.CtxElapse(ctx, fmt.Sprintf("do query done, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID, req.GetFromShardLeader(), dmlChannel, req.GetSegmentIDs()))

	failRet.Status.ErrorCode = commonpb.ErrorCode_Success
	latency := tr.ElapseSpan()
	metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.Leader).Observe(float64(latency.Milliseconds()))
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.SuccessLabel).Inc()
	return ret, nil
}

// Query performs replica query tasks.
func (node *QueryNode) Query(ctx context.Context, req *querypb.QueryRequest) (*internalpb.RetrieveResults, error) {
	log.Ctx(ctx).Debug("Received QueryRequest",
		zap.Bool("fromShardleader", req.GetFromShardLeader()),
		zap.Strings("vChannels", req.GetDmlChannels()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
		zap.Uint64("guaranteeTimestamp", req.Req.GetGuaranteeTimestamp()),
		zap.Uint64("timeTravel", req.GetReq().GetTravelTimestamp()))

	if req.GetReq().GetBase().GetTargetID() != paramtable.GetNodeID() {
		return &internalpb.RetrieveResults{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
				Reason:    common.WrapNodeIDNotMatchMsg(req.GetReq().GetBase().GetTargetID(), paramtable.GetNodeID()),
			},
		}, nil
	}

	failRet := &internalpb.RetrieveResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
	}

	toMergeResults := make([]*internalpb.RetrieveResults, 0)
	runningGp, runningCtx := errgroup.WithContext(ctx)
	mu := &sync.Mutex{}

	for _, ch := range req.GetDmlChannels() {
		ch := ch
		req := &querypb.QueryRequest{
			Req:             req.Req,
			DmlChannels:     []string{ch},
			SegmentIDs:      req.SegmentIDs,
			FromShardLeader: req.FromShardLeader,
			Scope:           req.Scope,
		}
		runningGp.Go(func() error {
			ret, err := node.queryWithDmlChannel(runningCtx, req, ch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failRet.Status.Reason = err.Error()
				failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
				return err
			}
			if ret.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
				failRet.Status.Reason = ret.Status.Reason
				failRet.Status.ErrorCode = ret.Status.ErrorCode
				return fmt.Errorf("%s", ret.Status.Reason)
			}
			toMergeResults = append(toMergeResults, ret)
			return nil
		})
	}
	if err := runningGp.Wait(); err != nil {
		return failRet, nil
	}
	ret, err := mergeInternalRetrieveResult(ctx, toMergeResults, req.GetReq().GetLimit())
	if err != nil {
		failRet.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
		failRet.Status.Reason = err.Error()
		return failRet, nil
	}

	if !req.FromShardLeader {
		rateCol.Add(metricsinfo.NQPerSecond, 1)
		metrics.QueryNodeExecuteCounter.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), metrics.QueryLabel).Add(float64(proto.Size(req)))
	}
	return ret, nil
}

// SyncReplicaSegments syncs replica node & segments states
func (node *QueryNode) SyncReplicaSegments(ctx context.Context, req *querypb.SyncReplicaSegmentsRequest) (*commonpb.Status, error) {
	if !node.isHealthy() {
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    msgQueryNodeIsUnhealthy(paramtable.GetNodeID()),
		}, nil
	}

	log.Info("Received SyncReplicaSegments request", zap.String("vchannelName", req.GetVchannelName()))

	err := node.ShardClusterService.SyncReplicaSegments(req.GetVchannelName(), req.GetReplicaSegments())
	if err != nil {
		log.Warn("failed to sync replica semgents,", zap.String("vchannel", req.GetVchannelName()), zap.Error(err))
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}, nil
	}

	log.Info("SyncReplicaSegments Done", zap.String("vchannel", req.GetVchannelName()))

	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}, nil
}

// ShowConfigurations returns the configurations of queryNode matching req.Pattern
func (node *QueryNode) ShowConfigurations(ctx context.Context, req *internalpb.ShowConfigurationsRequest) (*internalpb.ShowConfigurationsResponse, error) {
	if !node.isHealthy() {
		log.Warn("QueryNode.ShowConfigurations failed",
			zap.Int64("nodeId", paramtable.GetNodeID()),
			zap.String("req", req.Pattern),
			zap.Error(errQueryNodeIsUnhealthy(paramtable.GetNodeID())))

		return &internalpb.ShowConfigurationsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgQueryNodeIsUnhealthy(paramtable.GetNodeID()),
			},
			Configuations: nil,
		}, nil
	}

	return getComponentConfigurations(ctx, req), nil
}

// GetMetrics return system infos of the query node, such as total memory, memory usage, cpu usage ...
func (node *QueryNode) GetMetrics(ctx context.Context, req *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	if !node.isHealthy() {
		log.Warn("QueryNode.GetMetrics failed",
			zap.Int64("nodeId", paramtable.GetNodeID()),
			zap.String("req", req.Request),
			zap.Error(errQueryNodeIsUnhealthy(paramtable.GetNodeID())))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgQueryNodeIsUnhealthy(paramtable.GetNodeID()),
			},
			Response: "",
		}, nil
	}

	metricType, err := metricsinfo.ParseMetricType(req.Request)
	if err != nil {
		log.Warn("QueryNode.GetMetrics failed to parse metric type",
			zap.Int64("nodeId", paramtable.GetNodeID()),
			zap.String("req", req.Request),
			zap.Error(err))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			},
		}, nil
	}

	if metricType == metricsinfo.SystemInfoMetrics {
		queryNodeMetrics, err := getSystemInfoMetrics(ctx, req, node)
		if err != nil {
			log.Warn("QueryNode.GetMetrics failed",
				zap.Int64("nodeId", paramtable.GetNodeID()),
				zap.String("req", req.Request),
				zap.String("metricType", metricType),
				zap.Error(err))
			return &milvuspb.GetMetricsResponse{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_UnexpectedError,
					Reason:    err.Error(),
				},
			}, nil
		}
		log.Debug("QueryNode.GetMetrics",
			zap.Int64("node_id", paramtable.GetNodeID()),
			zap.String("req", req.Request),
			zap.String("metric_type", metricType),
			zap.Any("queryNodeMetrics", queryNodeMetrics))

		return queryNodeMetrics, nil
	}

	log.Debug("QueryNode.GetMetrics failed, request metric type is not implemented yet",
		zap.Int64("nodeId", paramtable.GetNodeID()),
		zap.String("req", req.Request),
		zap.String("metricType", metricType))

	return &milvuspb.GetMetricsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    metricsinfo.MsgUnimplementedMetric,
		},
		Response: "",
	}, nil
}

func (node *QueryNode) GetDataDistribution(ctx context.Context, req *querypb.GetDataDistributionRequest) (*querypb.GetDataDistributionResponse, error) {
	log := log.With(
		zap.Int64("msg-id", req.GetBase().GetMsgID()),
		zap.Int64("node-id", paramtable.GetNodeID()),
	)
	if !node.isHealthy() {
		log.Warn("QueryNode.GetMetrics failed",
			zap.Error(errQueryNodeIsUnhealthy(paramtable.GetNodeID())))

		return &querypb.GetDataDistributionResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgQueryNodeIsUnhealthy(paramtable.GetNodeID()),
			},
		}, nil
	}

	// check target matches
	if req.GetBase().GetTargetID() != paramtable.GetNodeID() {
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(req.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return &querypb.GetDataDistributionResponse{Status: status}, nil
	}

	growingSegments := node.metaReplica.getGrowingSegments()
	sealedSegments := node.metaReplica.getSealedSegments()
	shardClusters := node.ShardClusterService.GetShardClusters()

	channelGrowingsMap := make(map[string]map[int64]*internalpb.MsgPosition)
	for _, s := range growingSegments {
		if _, ok := channelGrowingsMap[s.vChannelID]; !ok {
			channelGrowingsMap[s.vChannelID] = make(map[int64]*internalpb.MsgPosition)
		}

		channelGrowingsMap[s.vChannelID][s.ID()] = s.startPosition
	}

	segmentVersionInfos := make([]*querypb.SegmentVersionInfo, 0, len(sealedSegments))
	for _, s := range sealedSegments {
		info := &querypb.SegmentVersionInfo{
			ID:         s.ID(),
			Collection: s.collectionID,
			Partition:  s.partitionID,
			Channel:    s.vChannelID,
			Version:    s.version,
		}
		segmentVersionInfos = append(segmentVersionInfos, info)
	}

	channelVersionInfos := make([]*querypb.ChannelVersionInfo, 0, len(shardClusters))
	leaderViews := make([]*querypb.LeaderView, 0, len(shardClusters))
	for _, sc := range shardClusters {
		if !node.queryShardService.hasQueryShard(sc.vchannelName) {
			continue
		}
		segmentInfos := sc.GetSegmentInfos()
		mapping := make(map[int64]*querypb.SegmentDist)
		for _, info := range segmentInfos {
			mapping[info.segmentID] = &querypb.SegmentDist{
				NodeID:  info.nodeID,
				Version: info.version,
			}
		}
		view := &querypb.LeaderView{
			Collection:      sc.collectionID,
			Channel:         sc.vchannelName,
			SegmentDist:     mapping,
			GrowingSegments: channelGrowingsMap[sc.vchannelName],
		}
		leaderViews = append(leaderViews, view)

		channelInfo := &querypb.ChannelVersionInfo{
			Channel:    sc.vchannelName,
			Collection: sc.collectionID,
			Version:    sc.getVersion(),
		}
		channelVersionInfos = append(channelVersionInfos, channelInfo)
	}

	return &querypb.GetDataDistributionResponse{
		Status:      &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
		NodeID:      paramtable.GetNodeID(),
		Segments:    segmentVersionInfos,
		Channels:    channelVersionInfos,
		LeaderViews: leaderViews,
	}, nil
}

func (node *QueryNode) SyncDistribution(ctx context.Context, req *querypb.SyncDistributionRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(zap.Int64("collectionID", req.GetCollectionID()), zap.String("channel", req.GetChannel()))
	// check node healthy
	code := node.stateCode.Load().(commonpb.StateCode)
	if code != commonpb.StateCode_Healthy {
		err := fmt.Errorf("query node %d is not ready", paramtable.GetNodeID())
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}
		return status, nil
	}
	// check target matches
	if req.GetBase().GetTargetID() != paramtable.GetNodeID() {
		log.Warn("failed to do match target id when sync ", zap.Int64("expect", req.GetBase().GetTargetID()), zap.Int64("actual", node.session.ServerID))
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(req.GetBase().GetTargetID(), paramtable.GetNodeID()),
		}
		return status, nil
	}
	shardCluster, ok := node.ShardClusterService.getShardCluster(req.GetChannel())
	if !ok {
		log.Warn("failed to find shard cluster when sync ", zap.String("channel", req.GetChannel()))
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    "shard not exist",
		}, nil
	}
	for _, action := range req.GetActions() {
		log.Info("sync action", zap.String("Action", action.GetType().String()), zap.Int64("segmentID", action.SegmentID))
		switch action.GetType() {
		case querypb.SyncType_Remove:
			shardCluster.ReleaseSegments(ctx, &querypb.ReleaseSegmentsRequest{
				SegmentIDs: []UniqueID{action.GetSegmentID()},
				Scope:      querypb.DataScope_Historical,
			}, true)
		case querypb.SyncType_Set:
			shardCluster.SyncSegments([]*querypb.ReplicaSegmentsInfo{
				{
					NodeId:      action.GetNodeID(),
					PartitionId: action.GetPartitionID(),
					SegmentIds:  []int64{action.GetSegmentID()},
					Versions:    []int64{action.GetVersion()},
				},
			}, segmentStateLoaded)

		default:
			return &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    "unexpected action type",
			}, nil
		}
	}

	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
		Reason:    "",
	}, nil
}
