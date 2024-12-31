package inx

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/axonfibre/fibre.go/lo"
	"github.com/axonfibre/fibre.go/runtime/contextutils"
	"github.com/axonfibre/fibre.go/runtime/event"
	"github.com/axonfibre/fibre.go/runtime/workerpool"
	"github.com/axonfibre/fibre.go/serializer/v2"
	"github.com/iotaledger/hornet/v2/pkg/common"
	"github.com/iotaledger/hornet/v2/pkg/model/storage"
	"github.com/iotaledger/hornet/v2/pkg/tangle"
	"github.com/iotaledger/hornet/v2/pkg/tipselect"
	inx "github.com/iotaledger/inx/go"
	iotago "github.com/iotaledger/iota.go/v3"
)

func NewINXBlockMetadata(ctx context.Context, blockID iotago.BlockID, metadata *storage.BlockMetadata, tip ...*tipselect.Tip) (*inx.BlockMetadata, error) {
	m := &inx.BlockMetadata{
		BlockId: inx.NewBlockId(blockID),
		Parents: inx.NewBlockIds(metadata.Parents()),
		Solid:   metadata.IsSolid(),
	}

	referenced, msIndex, wfIndex := metadata.ReferencedWithIndexAndWhiteFlagIndex()
	if referenced {
		m.ReferencedByMilestoneIndex = msIndex
		m.WhiteFlagIndex = wfIndex
		inclusionState := inx.BlockMetadata_LEDGER_INCLUSION_STATE_NO_TRANSACTION
		conflict := metadata.Conflict()
		if conflict != storage.ConflictNone {
			inclusionState = inx.BlockMetadata_LEDGER_INCLUSION_STATE_CONFLICTING
			m.ConflictReason = inx.BlockMetadata_ConflictReason(conflict)
		} else if metadata.IsIncludedTxInLedger() {
			inclusionState = inx.BlockMetadata_LEDGER_INCLUSION_STATE_INCLUDED
		}
		m.LedgerInclusionState = inclusionState

		if metadata.IsMilestone() {
			cachedBlock := deps.Storage.CachedBlockOrNil(blockID) // block +1
			if cachedBlock == nil {
				return nil, status.Errorf(codes.NotFound, "block not found: %s", blockID.ToHex())
			}
			defer cachedBlock.Release(true) // block -1

			milestone := cachedBlock.Block().Milestone()
			if milestone == nil {
				return nil, status.Errorf(codes.NotFound, "milestone for block not found: %s", blockID.ToHex())
			}
			m.MilestoneIndex = milestone.Index
		}

		return m, nil
	}

	if metadata.IsSolid() {

		if len(tip) > 0 {
			switch tip[0].Score {
			case tipselect.ScoreLazy:
				// promote is false
				m.ShouldReattach = true
			case tipselect.ScoreSemiLazy:
				m.ShouldPromote = true
				// reattach is false
			case tipselect.ScoreNonLazy:
				// promote is false
				// reattach is false
			}

			return m, nil
		}

		// determine info about the quality of the tip if not referenced
		cmi := deps.SyncManager.ConfirmedMilestoneIndex()

		tipScore, err := deps.TipScoreCalculator.TipScore(ctx, blockID, cmi)
		if err != nil {
			if errors.Is(err, common.ErrOperationAborted) {
				return nil, status.Errorf(codes.Unavailable, err.Error())
			}

			return nil, status.Errorf(codes.Internal, err.Error())
		}

		switch tipScore {
		case tangle.TipScoreNotFound:
			return nil, status.Errorf(codes.Internal, "tip score could not be calculated")
		case tangle.TipScoreOCRIThresholdReached, tangle.TipScoreYCRIThresholdReached:
			m.ShouldPromote = true
			// reattach is false
		case tangle.TipScoreBelowMaxDepth:
			// promote is false
			m.ShouldReattach = true
		case tangle.TipScoreHealthy:
			// promote is false
			// reattach is false
		}
	}

	return m, nil
}

func (s *Server) ReadBlock(_ context.Context, blockID *inx.BlockId) (*inx.RawBlock, error) {
	blkID := blockID.Unwrap()
	cachedBlock := deps.Storage.CachedBlockOrNil(blkID) // block +1
	if cachedBlock == nil {
		return nil, status.Errorf(codes.NotFound, "block %s not found", blkID.ToHex())
	}
	defer cachedBlock.Release(true) // block -1

	return &inx.RawBlock{
		Data: cachedBlock.Block().Data(),
	}, nil
}

func (s *Server) ReadBlockMetadata(_ context.Context, blockID *inx.BlockId) (*inx.BlockMetadata, error) {
	blkID := blockID.Unwrap()
	cachedBlockMeta := deps.Storage.CachedBlockMetadataOrNil(blkID) // meta +1
	if cachedBlockMeta == nil {
		isSolidEntryPoint, err := deps.Storage.SolidEntryPointsContain(blkID)
		if err == nil && isSolidEntryPoint {
			return &inx.BlockMetadata{
				BlockId: blockID,
				Solid:   true,
			}, nil
		}

		return nil, status.Errorf(codes.NotFound, "block metadata %s not found", blkID.ToHex())
	}
	defer cachedBlockMeta.Release(true) // meta -1

	//nolint:contextcheck // we don't care if the client context has ended already (merging contexts would be too expensive here)
	return NewINXBlockMetadata(Component.Daemon().ContextStopped(), cachedBlockMeta.Metadata().BlockID(), cachedBlockMeta.Metadata())
}

func (s *Server) ListenToBlocks(_ *inx.NoParams, srv inx.INX_ListenToBlocksServer) error {
	ctx, cancel := context.WithCancel(Component.Daemon().ContextStopped())

	wp := workerpool.New("ListenToBlocks", workerCount).Start()

	unhook := deps.Tangle.Events.ReceivedNewBlock.Hook(func(cachedBlock *storage.CachedBlock, latestMilestoneIndex iotago.MilestoneIndex, confirmedMilestoneIndex iotago.MilestoneIndex) {
		defer cachedBlock.Release(true) // block -1

		payload := inx.NewBlockWithBytes(cachedBlock.Block().BlockID(), cachedBlock.Block().Data())
		if err := srv.Send(payload); err != nil {
			Component.LogErrorf("send error: %v", err)
			cancel()
		}
	}, event.WithWorkerPool(wp)).Unhook

	<-ctx.Done()
	unhook()

	// We need to wait until all tasks are done, otherwise we might call
	// "SendMsg" and "CloseSend" in parallel on the grpc stream, which is
	// not safe according to the grpc docs.
	wp.Shutdown()
	wp.ShutdownComplete.Wait()

	return ctx.Err()
}

func (s *Server) ListenToSolidBlocks(_ *inx.NoParams, srv inx.INX_ListenToSolidBlocksServer) error {
	ctx, cancel := context.WithCancel(Component.Daemon().ContextStopped())

	wp := workerpool.New("ListenToSolidBlocks", workerCount).Start()

	unhook := deps.Tangle.Events.BlockSolid.Hook(func(blockMeta *storage.CachedMetadata) {
		defer blockMeta.Release(true) // meta -1

		payload, err := NewINXBlockMetadata(ctx, blockMeta.Metadata().BlockID(), blockMeta.Metadata())
		if err != nil {
			Component.LogErrorf("serialize error: %v", err)
			cancel()

			return
		}

		if err := srv.Send(payload); err != nil {
			Component.LogErrorf("send error: %v", err)
			cancel()
		}
	}, event.WithWorkerPool(wp)).Unhook

	<-ctx.Done()
	unhook()

	// We need to wait until all tasks are done, otherwise we might call
	// "SendMsg" and "CloseSend" in parallel on the grpc stream, which is
	// not safe according to the grpc docs.
	wp.Shutdown()
	wp.ShutdownComplete.Wait()

	return ctx.Err()
}

func (s *Server) ListenToReferencedBlocks(_ *inx.NoParams, srv inx.INX_ListenToReferencedBlocksServer) error {
	ctx, cancel := context.WithCancel(Component.Daemon().ContextStopped())

	wp := workerpool.New("ListenToReferencedBlocks", workerCount).Start()

	unhook := deps.Tangle.Events.BlockReferenced.Hook(func(blockMeta *storage.CachedMetadata, index iotago.MilestoneIndex, confTime uint32) {
		defer blockMeta.Release(true) // meta -1

		payload, err := NewINXBlockMetadata(ctx, blockMeta.Metadata().BlockID(), blockMeta.Metadata())
		if err != nil {
			Component.LogErrorf("serialize error: %v", err)
			cancel()

			return
		}

		if err := srv.Send(payload); err != nil {
			Component.LogErrorf("send error: %v", err)
			cancel()
		}
	}, event.WithWorkerPool(wp)).Unhook

	<-ctx.Done()
	unhook()

	// We need to wait until all tasks are done, otherwise we might call
	// "SendMsg" and "CloseSend" in parallel on the grpc stream, which is
	// not safe according to the grpc docs.
	wp.Shutdown()
	wp.ShutdownComplete.Wait()

	return ctx.Err()
}

func (s *Server) ListenToTipScoreUpdates(_ *inx.NoParams, srv inx.INX_ListenToTipScoreUpdatesServer) error {
	ctx, cancel := context.WithCancel(Component.Daemon().ContextStopped())

	wp := workerpool.New("ListenToTipScoreUpdates", workerCount).Start()

	onTipAddedOrRemoved := func(tip *tipselect.Tip) {
		blockMeta := deps.Storage.CachedBlockMetadataOrNil(tip.BlockID)
		if blockMeta == nil {
			return
		}
		defer blockMeta.Release(true) // meta -1

		payload, err := NewINXBlockMetadata(ctx, blockMeta.Metadata().BlockID(), blockMeta.Metadata(), tip)
		if err != nil {
			Component.LogErrorf("serialize error: %v", err)
			cancel()

			return
		}
		if err := srv.Send(payload); err != nil {
			Component.LogErrorf("send error: %v", err)
			cancel()
		}
	}

	wp.Start()
	unhook := lo.Batch(
		deps.TipSelector.Events.TipAdded.Hook(onTipAddedOrRemoved, event.WithWorkerPool(wp)).Unhook,
		deps.TipSelector.Events.TipRemoved.Hook(onTipAddedOrRemoved, event.WithWorkerPool(wp)).Unhook,
	)

	<-ctx.Done()
	unhook()

	// We need to wait until all tasks are done, otherwise we might call
	// "SendMsg" and "CloseSend" in parallel on the grpc stream, which is
	// not safe according to the grpc docs.
	wp.Shutdown()
	wp.ShutdownComplete.Wait()

	return ctx.Err()
}

func (s *Server) SubmitBlock(ctx context.Context, rawBlock *inx.RawBlock) (*inx.BlockId, error) {
	block, err := rawBlock.UnwrapBlock(serializer.DeSeriModeNoValidation, nil)
	if err != nil {
		return nil, err
	}

	mergedCtx, mergedCtxCancel := contextutils.MergeContexts(ctx, Component.Daemon().ContextStopped())
	defer mergedCtxCancel()

	blockID, err := attacher.AttachBlock(mergedCtx, block)
	if err != nil {
		switch {
		case errors.Is(err, tangle.ErrBlockAttacherInvalidBlock):
			return nil, status.Errorf(codes.InvalidArgument, "failed to attach block: %s", err.Error())

		case errors.Is(err, tangle.ErrBlockAttacherAttachingNotPossible):
			return nil, status.Errorf(codes.Internal, "failed to attach block: %s", err.Error())

		case errors.Is(err, tangle.ErrBlockAttacherPoWNotAvailable):
			return nil, status.Errorf(codes.Unavailable, "failed to attach block: %s", err.Error())

		default:
			return nil, status.Errorf(codes.Internal, "failed to attach block: %s", err.Error())
		}
	}

	return inx.NewBlockId(blockID), nil
}
