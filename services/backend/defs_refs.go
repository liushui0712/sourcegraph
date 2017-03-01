package backend

import (
	"context"
	"encoding/json"
	"fmt"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sourcegraph/go-langserver/pkg/lsp"
	"github.com/sourcegraph/go-langserver/pkg/lspext"

	"sourcegraph.com/sourcegraph/sourcegraph/api/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/inventory"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/rcache"
	"sourcegraph.com/sourcegraph/sourcegraph/services/backend/internal/localstore"
	"sourcegraph.com/sourcegraph/sourcegraph/xlang"
)

var Defs = &defs{}

type defs struct{}

// totalRefsCache is a redis cache to avoid some queries for popular
// repositories (which can take ~1s) from causing any serious performance
// issues when the request rate is high.
var (
	totalRefsCache        = rcache.NewWithTTL("totalrefs", 3600) // 1h
	totalRefsCacheCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "src",
		Subsystem: "defs",
		Name:      "totalrefs_cache_hit",
		Help:      "Counts cache hits and misses for Defs.TotalRefs repo ref counts.",
	}, []string{"type"})
)

func init() {
	prometheus.MustRegister(totalRefsCacheCounter)
}

func (s *defs) TotalRefs(ctx context.Context, source string, inv *inventory.Inventory) (res int, err error) {
	if Mocks.Defs.TotalRefs != nil {
		return Mocks.Defs.TotalRefs(ctx, source)
	}

	ctx, done := trace(ctx, "Deps", "TotalRefs", source, &err)
	defer done()

	// Check if value is in the cache.
	jsonRes, ok := totalRefsCache.Get(source)
	if ok {
		totalRefsCacheCounter.WithLabelValues("hit").Inc()
		if err := json.Unmarshal(jsonRes, &res); err != nil {
			return 0, err
		}
		return res, nil
	}

	// Query value from the database.
	totalRefsCacheCounter.WithLabelValues("miss").Inc()
	res, err = localstore.GlobalDeps.TotalRefs(ctx, source, inv)
	if err != nil {
		return 0, err
	}

	// Store value in the cache.
	jsonRes, err = json.Marshal(res)
	if err != nil {
		return 0, err
	}
	totalRefsCache.Set(source, jsonRes)
	return res, nil
}

// Dependencies returns the dependency references for the given repoID. I.e., the repo's dependencies.
func (s *defs) Dependencies(ctx context.Context, repoID int32, excludePrivate bool) ([]*sourcegraph.DependencyReference, error) {
	if Mocks.Defs.Dependencies != nil {
		return Mocks.Defs.Dependencies(ctx, repoID, excludePrivate)
	}

	return localstore.GlobalDeps.Dependencies(ctx, localstore.DependenciesOptions{
		Repo:           repoID,
		ExcludePrivate: excludePrivate,
	})
}

func (s *defs) DependencyReferences(ctx context.Context, op sourcegraph.DependencyReferencesOptions) (res *sourcegraph.DependencyReferences, err error) {
	if Mocks.Defs.DependencyReferences != nil {
		return Mocks.Defs.DependencyReferences(ctx, op)
	}

	ctx, done := trace(ctx, "Defs", "RefLocations", op, &err)
	defer done()

	span := opentracing.SpanFromContext(ctx)
	span.SetTag("language", op.Language)
	span.SetTag("repo_id", op.RepoID)
	span.SetTag("commit_id", op.CommitID)
	span.SetTag("file", op.File)
	span.SetTag("line", op.Line)
	span.SetTag("character", op.Character)

	// 🚨 SECURITY: We first must call textDocument/xdefinition on a ref 🚨
	// to figure out what to query the global deps database for. The
	// ref might exist in a private repo, so we MUST check that the
	// user has access to that private repo first prior to calling it
	// in xlang (xlang has unlimited, unchecked access to gitserver).
	//
	// For example, if a user is browsing a private repository but
	// looking for references to a public repository's symbol
	// (fmt.Println), we support that, but we DO NOT support looking
	// for references to a private repository's symbol ever (in fact,
	// they are not even indexed by the global deps database).
	//
	// 🚨 SECURITY: repository permissions are checked here 🚨
	//
	// The Repos.Get call here is responsible for ensuring the user has access
	// to the repository.
	repo, err := Repos.Get(ctx, &sourcegraph.RepoSpec{ID: op.RepoID})
	if err != nil {
		return nil, err
	}
	vcs := "git" // TODO: store VCS type in *sourcegraph.Repo object.
	span.SetTag("repo", repo.URI)

	// Determine the rootPath.
	rootPath := vcs + "://" + repo.URI + "?" + op.CommitID

	// Find the metadata for the definition specified by op, such that we can
	// perform the DB query using that metadata.
	var locations []lspext.SymbolLocationInformation
	err = xlang.UnsafeOneShotClientRequest(ctx, op.Language, rootPath, "textDocument/xdefinition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: rootPath + "#" + op.File},
		Position:     lsp.Position{Line: op.Line, Character: op.Character},
	}, &locations)
	if err != nil {
		return nil, errors.Wrap(err, "LSP textDocument/xdefinition")
	}
	if len(locations) == 0 {
		return nil, fmt.Errorf("textDocument/xdefinition returned zero locations")
	}

	// TODO(slimsag): figure out how to handle multiple location responses here
	// once we have a language server that uses it.
	location := locations[0]

	// If the symbol is not referenceable according to language semantics, then
	// there is no need to consult the database or perform roundtrips for
	// workspace/xreferences requests.
	var depRefs []*sourcegraph.DependencyReference
	if !xlang.IsSymbolReferenceable(op.Language, location.Symbol) {
		span.SetTag("nonreferencable", true)
	} else {
		pkgDescriptor, ok := xlang.SymbolPackageDescriptor(location.Symbol, op.Language)
		if !ok {
			return nil, err
		}

		depRefs, err = localstore.GlobalDeps.Dependencies(ctx, localstore.DependenciesOptions{
			Language: op.Language,
			DepData:  pkgDescriptor,
			Limit:    op.Limit,
		})
		if err != nil {
			return nil, err
		}
	}

	span.SetTag("# depRefs", len(depRefs))
	return &sourcegraph.DependencyReferences{
		References: depRefs,
		Location:   location,
	}, nil
}

// RefreshIndex refreshes the global deps index for the specified
// repository.
func (s *defs) RefreshIndex(ctx context.Context, repoURI, commitID string) (err error) {
	if Mocks.Defs.RefreshIndex != nil {
		return Mocks.Defs.RefreshIndex(ctx, repoURI, commitID)
	}

	ctx, done := trace(ctx, "Defs", "RefreshIndex", map[string]interface{}{"repoURI": repoURI, "commitID": commitID}, &err)
	defer done()
	return localstore.GlobalDeps.RefreshIndex(ctx, repoURI, commitID, Repos.GetInventory)
}

type MockDefs struct {
	TotalRefs            func(ctx context.Context, source string) (res int, err error)
	DependencyReferences func(ctx context.Context, op sourcegraph.DependencyReferencesOptions) (res *sourcegraph.DependencyReferences, err error)
	RefreshIndex         func(ctx context.Context, repoURI, commitID string) error
	Dependencies         func(ctx context.Context, repoID int32, excludePrivate bool) ([]*sourcegraph.DependencyReference, error)
}
