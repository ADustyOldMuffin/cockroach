// Copyright 2021 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package streamproducer

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl/streampb"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobsprotectedts"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts/ptpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/streaming"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
)

// startReplicationStreamJob initializes a replication stream producer job on the source cluster that
// 1. Tracks the liveness of the replication stream consumption
// 2. TODO(casper): Updates the protected timestamp for spans being replicated
func startReplicationStreamJob(
	evalCtx *tree.EvalContext, txn *kv.Txn, tenantID uint64,
) (streaming.StreamID, error) {
	execConfig := evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig)
	hasAdminRole, err := evalCtx.SessionAccessor.HasAdminRole(evalCtx.Ctx())

	if err != nil {
		return streaming.InvalidStreamID, err
	}

	if !hasAdminRole {
		return streaming.InvalidStreamID, errors.New("admin role required to start stream replication jobs")
	}

	registry := execConfig.JobRegistry
	timeout := streamingccl.StreamReplicationJobLivenessTimeout.Get(&evalCtx.Settings.SV)
	ptsID := uuid.MakeV4()
	jr := makeProducerJobRecord(registry, tenantID, timeout, evalCtx.SessionData().User(), ptsID)
	if _, err := registry.CreateAdoptableJobWithTxn(evalCtx.Ctx(), jr, jr.JobID, txn); err != nil {
		return streaming.InvalidStreamID, err
	}

	ptp := execConfig.ProtectedTimestampProvider
	statementTime := hlc.Timestamp{
		WallTime: evalCtx.GetStmtTimestamp().UnixNano(),
	}

	deprecatedSpansToProtect := roachpb.Spans{*makeTenantSpan(tenantID)}
	targetToProtect := ptpb.MakeTenantsTarget([]roachpb.TenantID{roachpb.MakeTenantID(tenantID)})

	pts := jobsprotectedts.MakeRecord(ptsID, int64(jr.JobID), statementTime,
		deprecatedSpansToProtect, jobsprotectedts.Jobs, targetToProtect)

	if err := ptp.Protect(evalCtx.Ctx(), txn, pts); err != nil {
		return streaming.InvalidStreamID, err
	}
	return streaming.StreamID(jr.JobID), nil
}

// updateReplicationStreamProgress updates the job progress for an active replication
// stream specified by 'streamID' and returns error if the stream is no longer active.
func updateReplicationStreamProgress(
	ctx context.Context,
	expiration time.Time,
	ptsProvider protectedts.Provider,
	registry *jobs.Registry,
	streamID streaming.StreamID,
	ts hlc.Timestamp,
	txn *kv.Txn,
) (status streampb.StreamReplicationStatus, err error) {
	const useReadLock = false
	err = registry.UpdateJobWithTxn(ctx, jobspb.JobID(streamID), txn, useReadLock,
		func(txn *kv.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			if md.Status == jobs.StatusRunning {
				status.StreamStatus = streampb.StreamReplicationStatus_STREAM_ACTIVE
			} else if md.Status == jobs.StatusPaused {
				status.StreamStatus = streampb.StreamReplicationStatus_STREAM_PAUSED
			} else if md.Status.Terminal() {
				status.StreamStatus = streampb.StreamReplicationStatus_STREAM_INACTIVE
			} else {
				status.StreamStatus = streampb.StreamReplicationStatus_UNKNOWN_STREAM_STATUS_RETRY
			}
			// Skip checking PTS record in cases that it might already be released
			if status.StreamStatus != streampb.StreamReplicationStatus_STREAM_ACTIVE &&
				status.StreamStatus != streampb.StreamReplicationStatus_STREAM_PAUSED {
				return nil
			}

			ptsID := *md.Payload.GetStreamReplication().ProtectedTimestampRecord
			ptsRecord, err := ptsProvider.GetRecord(ctx, txn, ptsID)
			if err != nil {
				return err
			}
			status.ProtectedTimestamp = &ptsRecord.Timestamp
			if status.StreamStatus != streampb.StreamReplicationStatus_STREAM_ACTIVE {
				return nil
			}

			if shouldUpdatePTS := ptsRecord.Timestamp.Less(ts); shouldUpdatePTS {
				if err = ptsProvider.UpdateTimestamp(ctx, txn, ptsID, ts); err != nil {
					return err
				}
				status.ProtectedTimestamp = &ts
			}

			if p := md.Progress; expiration.After(p.GetStreamReplication().Expiration) {
				p.GetStreamReplication().Expiration = expiration
				ju.UpdateProgress(p)
			}
			return nil
		})

	if jobs.HasJobNotFoundError(err) || testutils.IsError(err, "not found in system.jobs table") {
		status.StreamStatus = streampb.StreamReplicationStatus_STREAM_INACTIVE
		err = nil
	}

	return status, err
}

// heartbeatReplicationStream updates replication stream progress and advances protected timestamp
// record to the specified frontier.
func heartbeatReplicationStream(
	evalCtx *tree.EvalContext, streamID streaming.StreamID, frontier hlc.Timestamp, txn *kv.Txn,
) (streampb.StreamReplicationStatus, error) {

	execConfig := evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig)
	timeout := streamingccl.StreamReplicationJobLivenessTimeout.Get(&evalCtx.Settings.SV)
	expirationTime := timeutil.Now().Add(timeout)

	return updateReplicationStreamProgress(evalCtx.Ctx(),
		expirationTime, execConfig.ProtectedTimestampProvider, execConfig.JobRegistry, streamID, frontier, txn)
}

// getReplicationStreamSpec gets a replication stream specification for the specified stream.
func getReplicationStreamSpec(
	evalCtx *tree.EvalContext, txn *kv.Txn, streamID streaming.StreamID,
) (*streampb.ReplicationStreamSpec, error) {
	jobExecCtx := evalCtx.JobExecContext.(sql.JobExecContext)
	// Returns error if the replication stream is not active
	j, err := jobExecCtx.ExecCfg().JobRegistry.LoadJob(evalCtx.Ctx(), jobspb.JobID(streamID))
	if err != nil {
		return nil, errors.Wrapf(err, "Replication stream %d has error", streamID)
	}
	if j.Status() != jobs.StatusRunning {
		return nil, errors.Errorf("Replication stream %d is not running", streamID)
	}

	// Partition the spans with SQLPlanner
	var noTxn *kv.Txn
	dsp := jobExecCtx.DistSQLPlanner()
	planCtx := dsp.NewPlanningCtx(evalCtx.Ctx(), jobExecCtx.ExtendedEvalContext(),
		nil /* planner */, noTxn, sql.DistributionTypeSystemTenantOnly)

	replicatedSpans := j.Details().(jobspb.StreamReplicationDetails).Spans
	spans := make([]roachpb.Span, 0, len(replicatedSpans))
	for _, span := range replicatedSpans {
		spans = append(spans, *span)
	}
	spanPartitions, err := dsp.PartitionSpans(evalCtx.Ctx(), planCtx, spans)
	if err != nil {
		return nil, err
	}

	res := &streampb.ReplicationStreamSpec{
		Partitions: make([]streampb.ReplicationStreamSpec_Partition, 0, len(spanPartitions)),
	}
	for _, sp := range spanPartitions {
		nodeInfo, err := dsp.GetSQLInstanceInfo(sp.SQLInstanceID)
		if err != nil {
			return nil, err
		}
		res.Partitions = append(res.Partitions, streampb.ReplicationStreamSpec_Partition{
			NodeID:     roachpb.NodeID(sp.SQLInstanceID),
			SQLAddress: nodeInfo.SQLAddress,
			Locality:   nodeInfo.Locality,
			PartitionSpec: &streampb.StreamPartitionSpec{
				Spans: sp.Spans,
				Config: streampb.StreamPartitionSpec_ExecutionConfig{
					MinCheckpointFrequency: streamingccl.StreamReplicationMinCheckpointFrequency.Get(&evalCtx.Settings.SV),
				},
			},
		})
	}
	return res, nil
}

func completeReplicationStream(
	evalCtx *tree.EvalContext, txn *kv.Txn, streamID streaming.StreamID,
) error {
	// Update the producer job that a cutover happens on the consumer side.
	registry := evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig).JobRegistry
	const useReadLock = false
	return registry.UpdateJobWithTxn(evalCtx.Ctx(), jobspb.JobID(streamID), txn, useReadLock,
		func(txn *kv.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			if (md.Status == jobs.StatusRunning || md.Status == jobs.StatusPending) &&
				!md.Progress.GetStreamReplication().IngestionCutOver {
				p := md.Progress
				p.GetStreamReplication().IngestionCutOver = true
				ju.UpdateProgress(p)
			}
			return nil
		})
}
