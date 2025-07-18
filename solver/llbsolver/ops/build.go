package ops

import (
	"context"
	"encoding/json"
	"os"

	"github.com/containerd/continuity/fs"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/ops/opsutils"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/cachedigest"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const buildCacheType = "buildkit.build.v0"

type BuildOp struct {
	op *pb.BuildOp
	b  frontend.FrontendLLBBridge
	v  solver.Vertex
}

var _ solver.Op = &BuildOp{}

func NewBuildOp(v solver.Vertex, op *pb.Op_Build, b frontend.FrontendLLBBridge, _ worker.Worker) (*BuildOp, error) {
	if err := opsutils.Validate(&pb.Op{Op: op}); err != nil {
		return nil, err
	}
	return &BuildOp{
		op: op.Build,
		b:  b,
		v:  v,
	}, nil
}

func (b *BuildOp) CacheMap(ctx context.Context, g session.Group, index int) (*solver.CacheMap, bool, error) {
	dt, err := json.Marshal(struct {
		Type string
		Exec *pb.BuildOp
	}{
		Type: buildCacheType,
		Exec: b.op,
	})
	if err != nil {
		return nil, false, err
	}

	dgst, err := cachedigest.FromBytes(dt, cachedigest.TypeJSON)
	if err != nil {
		return nil, false, err
	}
	return &solver.CacheMap{
		Digest: dgst,
		Deps: make([]struct {
			Selector          digest.Digest
			ComputeDigestFunc solver.ResultBasedCacheFunc
			PreprocessFunc    solver.PreprocessFunc
		}, len(b.v.Inputs())),
	}, true, nil
}

func (b *BuildOp) Exec(ctx context.Context, g session.Group, inputs []solver.Result) (outputs []solver.Result, retErr error) {
	if b.op.Builder != int64(pb.LLBBuilder) {
		return nil, errors.Errorf("only LLB builder is currently allowed")
	}

	builderInputs := b.op.Inputs
	llbDef, ok := builderInputs[pb.LLBDefinitionInput]
	if !ok {
		return nil, errors.Errorf("no llb definition input %s found", pb.LLBDefinitionInput)
	}

	i := int(llbDef.Input)
	if i >= len(inputs) {
		return nil, errors.Errorf("invalid index %v", i) // TODO: this should be validated before
	}
	inp := inputs[i]

	ref, ok := inp.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid reference for build %T", inp.Sys())
	}

	mount, err := ref.ImmutableRef.Mount(ctx, true, g)
	if err != nil {
		return nil, err
	}

	lm := snapshot.LocalMounter(mount)

	root, err := lm.Mount()
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	fn := pb.LLBDefaultDefinitionFile
	if override, ok := b.op.Attrs[pb.AttrLLBDefinitionFilename]; ok {
		fn = override
	}

	newfn, err := fs.RootPath(root, fn)
	if err != nil {
		return nil, errors.Wrapf(err, "working dir %s points to invalid target", fn)
	}

	f, err := os.Open(newfn)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open %s", newfn)
	}

	def, err := llb.ReadFrom(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	lm.Unmount()
	lm = nil

	newRes, err := b.b.Solve(ctx, frontend.SolveRequest{
		Definition: def.ToPB(),
	}, g.SessionIterator().NextSession())
	if err != nil {
		return nil, err
	}

	newRes.EachRef(func(ref solver.ResultProxy) error {
		if ref == newRes.Ref {
			return nil
		}
		return ref.Release(context.TODO())
	})

	r, err := newRes.Ref.Result(ctx)
	if err != nil {
		return nil, err
	}

	return []solver.Result{r}, err
}

func (b *BuildOp) Acquire(ctx context.Context) (solver.ReleaseFunc, error) {
	// buildOp itself does not count towards parallelism budget.
	return func() {}, nil
}

func (b *BuildOp) IsProvenanceProvider() {}
