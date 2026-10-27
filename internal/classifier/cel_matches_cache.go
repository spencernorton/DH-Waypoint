package classifier

import (
	"regexp"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"
	lru "github.com/hashicorp/golang-lru/v2"
)

// The CEL stdlib `matches` compiles its pattern on every Eval. Workflow
// conditions call it per torrent — often inside keywordList folds, where
// the pattern is a loop variable — which made regexp compilation the
// dominant allocation in the classification pipeline (~73% of bytes in
// BenchmarkDeterministicPipeline). Patterns come from workflow text and
// keyword constants, so a small bounded cache removes the recompile
// without changing match semantics; compile errors are not cached and
// surface exactly as the stdlib returns them.
//
// ponytail: 512-entry LRU; raise only if a workflow legitimately uses
// more distinct patterns than that.
var matchesCache, _ = lru.New[string, *regexp.Regexp](512)

func cachedMatches(values ...ref.Val) ref.Val {
	if len(values) != 2 {
		return types.NoSuchOverloadErr()
	}
	in, ok := values[0].Value().(string)
	if !ok {
		return types.MaybeNoSuchOverloadErr(values[0])
	}
	pat, ok := values[1].Value().(string)
	if !ok {
		return types.MaybeNoSuchOverloadErr(values[1])
	}
	re, ok := matchesCache.Get(pat)
	if !ok {
		var err error
		re, err = regexp.Compile(pat)
		if err != nil {
			return types.NewErrFromString(err.Error())
		}
		matchesCache.Add(pat, re)
	}
	return types.Bool(re.MatchString(in))
}

// cacheMatchesDecorator wraps every `matches` call in a compiled CEL
// program with cachedMatches. It complements cel.OptimizeRegex, which
// only folds constant patterns; fold variables still recompile without
// this decorator.
func cacheMatchesDecorator(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
	call, ok := i.(interpreter.InterpretableCall)
	if !ok || call.Function() != "matches" || len(call.Args()) != 2 {
		return i, nil
	}
	return interpreter.NewCall(call.ID(), call.Function(), call.OverloadID(), call.Args(), cachedMatches), nil
}
