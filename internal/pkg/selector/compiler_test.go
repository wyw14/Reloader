package selector

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func view(labels, annotations map[string]string) NamespaceView {
	return NamespaceView{Name: "test-ns", Labels: labels, Annotations: annotations}
}

func TestCompileEmptySelector(t *testing.T) {
	t.Run("zero spec matches everything including nil maps", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{Version: "v1"})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())
		assert.True(t, predicate.Empty())
		assert.Equal(t, "v1", predicate.Version())
		assert.Equal(t, "", predicate.String())

		assert.True(t, predicate.Matches(NamespaceView{}))
		assert.True(t, predicate.Matches(view(map[string]string{"env": "prod"}, nil)))
		assert.True(t, predicate.Matches(view(nil, map[string]string{"any": "thing"})))
	})

	t.Run("whitespace-only label selector is empty", func(t *testing.T) {
		predicate, emptyDiags := Compile(Spec{LabelSelector: "   \t "})
		require.NotNil(t, predicate)
		assert.False(t, emptyDiags.HasErrors())
		assert.True(t, predicate.Empty())
	})

	t.Run("empty annotation requirement list still matches", func(t *testing.T) {
		predicate, emptyDiags := Compile(Spec{ResourceAnnotations: []AnnotationRequirement{}})
		require.NotNil(t, predicate)
		assert.False(t, emptyDiags.HasErrors())
		assert.True(t, predicate.Empty())
		assert.True(t, predicate.Matches(view(nil, nil)))
	})
}

func TestCompileLabelRequirements(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		view     NamespaceView
		expected bool
	}{
		{
			name:     "equals match",
			selector: "env=prod",
			view:     view(map[string]string{"env": "prod"}, nil),
			expected: true,
		},
		{
			name:     "equals mismatch",
			selector: "env=prod",
			view:     view(map[string]string{"env": "dev"}, nil),
			expected: false,
		},
		{
			name:     "double equals match",
			selector: "env==prod",
			view:     view(map[string]string{"env": "prod"}, nil),
			expected: true,
		},
		{
			name:     "exists match",
			selector: "team",
			view:     view(map[string]string{"team": "platform"}, nil),
			expected: true,
		},
		{
			name:     "exists misses absent key",
			selector: "team",
			view:     view(map[string]string{}, nil),
			expected: false,
		},
		{
			name:     "in match",
			selector: "tier in (web, api)",
			view:     view(map[string]string{"tier": "api"}, nil),
			expected: true,
		},
		{
			name:     "in mismatch",
			selector: "tier in (web, api)",
			view:     view(map[string]string{"tier": "batch"}, nil),
			expected: false,
		},
		{
			name:     "whitespace tolerant",
			selector: "env = prod , tier in( web )",
			view:     view(map[string]string{"env": "prod", "tier": "web"}, nil),
			expected: true,
		},
		{
			name:     "greater than match",
			selector: "reload.weight>10",
			view:     view(map[string]string{"reload.weight": "11"}, nil),
			expected: true,
		},
		{
			name:     "greater than boundary misses",
			selector: "reload.weight>10",
			view:     view(map[string]string{"reload.weight": "10"}, nil),
			expected: false,
		},
		{
			name:     "less than match",
			selector: "reload.weight<3",
			view:     view(map[string]string{"reload.weight": "2"}, nil),
			expected: true,
		},
		{
			name:     "non numeric value fails gt match",
			selector: "reload.weight>10",
			view:     view(map[string]string{"reload.weight": "soon"}, nil),
			expected: false,
		},
		{
			name:     "combined requirements are AND-ed",
			selector: "env=prod,tier=web",
			view:     view(map[string]string{"env": "prod", "tier": "web"}, nil),
			expected: true,
		},
		{
			name:     "combined requirements reject partial match",
			selector: "env=prod,tier=web",
			view:     view(map[string]string{"env": "prod", "tier": "batch"}, nil),
			expected: false,
		},
		{
			name:     "qualified key supported",
			selector: "reloader.stakater.com/managed=true",
			view:     view(map[string]string{"reloader.stakater.com/managed": "true"}, nil),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicate, diagnostics := Compile(Spec{Version: "test", LabelSelector: tt.selector})
			require.NotNil(t, predicate, "compilation errors: %s", diagnostics.Error())
			assert.False(t, diagnostics.HasErrors(), diagnostics.Error())
			assert.False(t, predicate.Empty())
			assert.Equal(t, tt.expected, predicate.Matches(tt.view))
		})
	}
}

func TestCompileNegation(t *testing.T) {
	t.Run("doesnotexist matches when key absent", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{LabelSelector: "!canary"})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(map[string]string{"stable": "yes"}, nil)))
		assert.True(t, predicate.Matches(view(map[string]string{}, nil)))
		assert.False(t, predicate.Matches(view(map[string]string{"canary": "true"}, nil)))
	})

	t.Run("not equals matches absent key and different values", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{LabelSelector: "tier!=canary"})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(map[string]string{}, nil)), "absent key satisfies !=")
		assert.True(t, predicate.Matches(view(map[string]string{"tier": "web"}, nil)))
		assert.False(t, predicate.Matches(view(map[string]string{"tier": "canary"}, nil)))
	})

	t.Run("not in matches absent and outside values", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{LabelSelector: "tier notin (canary, debug)"})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(map[string]string{}, nil)), "absent key satisfies notin")
		assert.True(t, predicate.Matches(view(map[string]string{"tier": "web"}, nil)))
		assert.False(t, predicate.Matches(view(map[string]string{"tier": "debug"}, nil)))
	})

	t.Run("negation combined with positive requirement", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{LabelSelector: "env=prod,!frozen"})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(map[string]string{"env": "prod"}, nil)))
		assert.False(t, predicate.Matches(view(map[string]string{"env": "prod", "frozen": "true"}, nil)))
		assert.False(t, predicate.Matches(view(map[string]string{"env": "dev"}, nil)))
	})

	t.Run("annotation doesnotexist and notequals", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{ResourceAnnotations: []AnnotationRequirement{
			{Key: "reloader.stakater.com/paused", Operator: "doesnotexist"},
			{Key: "reloader.stakater.com/window", Operator: "!=", Values: []string{"closed"}},
		}})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(nil, map[string]string{})))
		assert.True(t, predicate.Matches(view(nil, map[string]string{"reloader.stakater.com/window": "open"})))
		assert.False(t, predicate.Matches(view(nil, map[string]string{"reloader.stakater.com/paused": "true"})))
		assert.False(t, predicate.Matches(view(nil, map[string]string{"reloader.stakater.com/window": "closed"})))
	})

	t.Run("annotation notin", func(t *testing.T) {
		predicate, diagnostics := Compile(Spec{ResourceAnnotations: []AnnotationRequirement{
			{Key: "zone", Operator: "notin", Values: []string{"dark", "offline"}},
		}})
		require.NotNil(t, predicate)
		assert.False(t, diagnostics.HasErrors())

		assert.True(t, predicate.Matches(view(nil, nil)))
		assert.True(t, predicate.Matches(view(nil, map[string]string{"zone": "lit"})))
		assert.False(t, predicate.Matches(view(nil, map[string]string{"zone": "dark"})))
	})
}

func TestCompileResourceAnnotations(t *testing.T) {
	predicate, diagnostics := Compile(Spec{ResourceAnnotations: []AnnotationRequirement{
		{Key: "reloader.stakater.com/enabled", Operator: "equals", Values: []string{"true"}},
		{Key: "reloader.stakater.com/reason", Operator: "in", Values: []string{"config", "secret"}},
	}})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors())
	assert.False(t, predicate.Empty())

	assert.True(t, predicate.Matches(view(nil, map[string]string{
		"reloader.stakater.com/enabled": "true",
		"reloader.stakater.com/reason":  "config",
	})))
	assert.False(t, predicate.Matches(view(nil, map[string]string{
		"reloader.stakater.com/enabled": "false",
		"reloader.stakater.com/reason":  "config",
	})))
	assert.False(t, predicate.Matches(view(nil, nil)))

	// Annotation values are free-form; spaces and punctuation must be accepted.
	freeForm, freeDiags := Compile(Spec{ResourceAnnotations: []AnnotationRequirement{
		{Key: "example.com/note", Operator: "=", Values: []string{"hello world, v2!"}},
	}})
	require.NotNil(t, freeForm, freeDiags.Error())
	assert.True(t, freeForm.Matches(view(nil, map[string]string{"example.com/note": "hello world, v2!"})))
}

func TestCompileLabelsAndAnnotationsTogether(t *testing.T) {
	predicate, diagnostics := Compile(Spec{
		LabelSelector: "env=prod",
		ResourceAnnotations: []AnnotationRequirement{
			{Key: "reloader.stakater.com/enabled", Operator: "exists"},
		},
	})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors())

	assert.True(t, predicate.Matches(view(
		map[string]string{"env": "prod"},
		map[string]string{"reloader.stakater.com/enabled": "true"},
	)))
	assert.False(t, predicate.Matches(view(
		map[string]string{"env": "dev"},
		map[string]string{"reloader.stakater.com/enabled": "true"},
	)))
	assert.False(t, predicate.Matches(view(
		map[string]string{"env": "prod"},
		map[string]string{},
	)))

	assert.Contains(t, predicate.String(), "env=prod")
	assert.Contains(t, predicate.String(), "exists")
}

func TestIllegalOperatorFieldLocation(t *testing.T) {
	tests := []struct {
		name          string
		spec          Spec
		expectedField string
	}{
		{
			name:          "tilde equals in first requirement",
			spec:          Spec{LabelSelector: "env~=prod"},
			expectedField: "labelSelector.requirements[0].operator",
		},
		{
			name:          "greater than or equal",
			spec:          Spec{LabelSelector: "env>=1"},
			expectedField: "labelSelector.requirements[0].operator",
		},
		{
			name:          "less than or equal",
			spec:          Spec{LabelSelector: "env<=1"},
			expectedField: "labelSelector.requirements[0].operator",
		},
		{
			name:          "illegal operator in second requirement keeps index",
			spec:          Spec{LabelSelector: "env=prod,tier~=web"},
			expectedField: "labelSelector.requirements[1].operator",
		},
		{
			name:          "uppercase IN keyword is an operator error",
			spec:          Spec{LabelSelector: "tier IN (web)"},
			expectedField: "labelSelector.requirements[0].operator",
		},
		{
			name:          "dangling not equals points at value field",
			spec:          Spec{LabelSelector: "env!="},
			expectedField: "labelSelector.requirements[0].values[0]",
		},
		{
			name:          "in without parenthesis points at values",
			spec:          Spec{LabelSelector: "tier in web"},
			expectedField: "labelSelector.requirements[0].values",
		},
		{
			name:          "invalid key points at key field",
			spec:          Spec{LabelSelector: "-leading=prod"},
			expectedField: "labelSelector.requirements[0].key",
		},
		{
			name: "annotation unsupported operator",
			spec: Spec{ResourceAnnotations: []AnnotationRequirement{
				{Key: "example.com/zone", Operator: "~=", Values: []string{"x"}},
			}},
			expectedField: "resourceAnnotations[0].operator",
		},
		{
			name: "annotation invalid key",
			spec: Spec{ResourceAnnotations: []AnnotationRequirement{
				{Key: "bad key", Operator: "exists"},
			}},
			expectedField: "resourceAnnotations[0].key",
		},
		{
			name: "annotation wrong arity points at values",
			spec: Spec{ResourceAnnotations: []AnnotationRequirement{
				{Key: "example.com/zone", Operator: "exists", Values: []string{"x"}},
			}},
			expectedField: "resourceAnnotations[0].values",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicate, diagnostics := Compile(tt.spec)
			assert.Nil(t, predicate, "no predicate should be produced for invalid spec")
			assert.True(t, diagnostics.HasErrors())
			found := false
			for _, diagnostic := range diagnostics {
				if diagnostic.Field == tt.expectedField {
					found = true
					assert.Equal(t, SeverityError, diagnostic.Severity)
				}
			}
			assert.Truef(t, found, "expected a diagnostic at %q, got: %s", tt.expectedField, diagnostics.Error())
		})
	}
}

func TestIllegalOperatorCollectsEveryRequirement(t *testing.T) {
	_, diagnostics := Compile(Spec{LabelSelector: "a~=1,b=2,c~=3"})
	assert.True(t, diagnostics.HasErrors())

	fields := map[string]bool{}
	for _, diagnostic := range diagnostics {
		fields[diagnostic.Field] = true
	}
	assert.True(t, fields["labelSelector.requirements[0].operator"])
	assert.True(t, fields["labelSelector.requirements[2].operator"])
}

func TestConflictDiagnostics(t *testing.T) {
	tests := []struct {
		name             string
		selector         string
		expectedField    string
		expectsNoMatches bool
	}{
		{
			name:             "two distinct equals",
			selector:         "env=prod,env=dev",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
		{
			name:             "exists and doesnotexist",
			selector:         "env,!env",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
		{
			name:             "equals contradicted by notequals",
			selector:         "env=prod,env!=prod",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
		{
			name:             "in fully covered by notin",
			selector:         "tier in (web,api),tier notin (web,api)",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
		{
			name:             "disjoint in sets",
			selector:         "tier in (web,api),tier in (batch)",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
		{
			name:             "impossible numeric window",
			selector:         "weight>5,weight<5",
			expectedField:    "labelSelector.requirements[1]",
			expectsNoMatches: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicate, diagnostics := Compile(Spec{LabelSelector: tt.selector})
			require.NotNil(t, predicate, "conflicting but syntactically valid selectors still compile")
			assert.True(t, diagnostics.HasErrors(), "expected conflict diagnostics")

			found := false
			for _, diagnostic := range diagnostics {
				if diagnostic.Field == tt.expectedField {
					found = true
					assert.Contains(t, diagnostic.Message, "conflict with labelSelector.requirements[0]")
				}
			}
			assert.Truef(t, found, "expected conflict at %q; got %s", tt.expectedField, diagnostics.Error())

			if tt.expectsNoMatches {
				assert.False(t, predicate.Matches(view(map[string]string{"env": "prod", "tier": "web", "weight": "6"}, nil)))
				assert.False(t, predicate.Matches(view(map[string]string{"env": "dev", "tier": "api", "weight": "4"}, nil)))
				assert.False(t, predicate.Matches(view(map[string]string{}, nil)))
			}
		})
	}
}

func TestSatisfiableOverlapIsNotAConflict(t *testing.T) {
	// in (web,api) minus notin (web) still permits "api".
	predicate, diagnostics := Compile(Spec{LabelSelector: "tier in (web,api),tier notin (web)"})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors(), diagnostics.Error())

	assert.True(t, predicate.Matches(view(map[string]string{"tier": "api"}, nil)))
	assert.False(t, predicate.Matches(view(map[string]string{"tier": "web"}, nil)))
}

func TestDuplicateRequirementIsAWarning(t *testing.T) {
	predicate, diagnostics := Compile(Spec{LabelSelector: "env=prod,env==prod"})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors(), "duplicates are warnings, not errors")

	warnings := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == SeverityWarning {
			warnings++
			assert.Equal(t, "labelSelector.requirements[1]", diagnostic.Field)
			assert.Contains(t, diagnostic.Message, "duplicate requirement")
		}
	}
	assert.Equal(t, 1, warnings)
	assert.True(t, predicate.Matches(view(map[string]string{"env": "prod"}, nil)))
}

func TestLabelAndAnnotationKeysDoNotConflict(t *testing.T) {
	// Same key name lives in two independent maps.
	predicate, diagnostics := Compile(Spec{
		LabelSelector: "env=prod,!other",
		ResourceAnnotations: []AnnotationRequirement{
			{Key: "env", Operator: "doesnotexist"},
		},
	})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors(), diagnostics.Error())
	assert.True(t, predicate.Matches(view(map[string]string{"env": "prod"}, map[string]string{})))
}

func TestVersionCache(t *testing.T) {
	compiler := NewCompiler()

	first, firstDiags := compiler.Compile(Spec{Version: "v1", LabelSelector: "env=prod"})
	require.NotNil(t, first)
	assert.False(t, firstDiags.HasErrors())

	second, _ := compiler.Compile(Spec{Version: "v1", LabelSelector: "env=prod"})
	assert.Same(t, first, second, "same version must return the cached predicate")

	// The cache is keyed on version only: callers are expected to bump the
	// version when selector content changes.
	sameVersionChangedContent, _ := compiler.Compile(Spec{Version: "v1", LabelSelector: "env=dev"})
	assert.Same(t, first, sameVersionChangedContent)

	third, _ := compiler.Compile(Spec{Version: "v2", LabelSelector: "env=dev"})
	assert.NotSame(t, first, third, "a new version invalidates the cached entry")
	assert.True(t, third.Matches(view(map[string]string{"env": "dev"}, nil)))

	// The previous immutable predicate remains usable after invalidation.
	assert.True(t, first.Matches(view(map[string]string{"env": "prod"}, nil)))
	assert.False(t, first.Matches(view(map[string]string{"env": "dev"}, nil)))
}

func TestVersionCacheInvalidation(t *testing.T) {
	compiler := NewCompiler()

	first, _ := compiler.Compile(Spec{Version: "v1", LabelSelector: "env=prod"})
	compiler.Invalidate()
	second, _ := compiler.Compile(Spec{Version: "v1", LabelSelector: "env=prod"})
	assert.NotSame(t, first, second, "Invalidate forces a recompile")
	assert.True(t, second.Matches(view(map[string]string{"env": "prod"}, nil)))
}

func TestFailedCompileIsCachedPerVersion(t *testing.T) {
	compiler := NewCompiler()

	bad, diags := compiler.Compile(Spec{Version: "bad-v1", LabelSelector: "env~=prod"})
	assert.Nil(t, bad)
	assert.True(t, diags.HasErrors())

	badAgain, diagsAgain := compiler.Compile(Spec{Version: "bad-v1", LabelSelector: "env~=prod"})
	assert.Nil(t, badAgain)
	assert.Equal(t, diags.Error(), diagsAgain.Error())

	fixed, fixedDiags := compiler.Compile(Spec{Version: "good-v2", LabelSelector: "env=prod"})
	require.NotNil(t, fixed)
	assert.False(t, fixedDiags.HasErrors())
}

func TestCompiledPredicateIsImmutableAgainstInputMutation(t *testing.T) {
	requirements := []AnnotationRequirement{
		{Key: "example.com/zone", Operator: "in", Values: []string{"a", "b"}},
	}
	predicate, diagnostics := Compile(Spec{ResourceAnnotations: requirements})
	require.NotNil(t, predicate)
	assert.False(t, diagnostics.HasErrors())

	requirements[0].Values[0] = "mutated"
	requirements[0].Operator = "exists"
	requirements = append(requirements, AnnotationRequirement{Key: "example.com/other", Operator: "exists"})
	assert.Len(t, requirements, 2)

	assert.True(t, predicate.Matches(view(nil, map[string]string{"example.com/zone": "a"})))
	assert.False(t, predicate.Matches(view(nil, map[string]string{"example.com/zone": "mutated"})))
}

func TestConcurrentCompileAndMatch(t *testing.T) {
	compiler := NewCompiler()

	specV1 := Spec{Version: "v1", LabelSelector: "env=prod,tier!=canary"}
	specV2 := Spec{Version: "v2", LabelSelector: "env=prod,tier!=canary,team in (core,infra)"}

	views := []NamespaceView{
		view(map[string]string{"env": "prod", "tier": "web"}, map[string]string{}),
		view(map[string]string{"env": "prod", "tier": "canary"}, map[string]string{}),
		view(map[string]string{"env": "dev", "tier": "web"}, map[string]string{}),
		view(nil, nil),
	}
	expectedV1 := []bool{true, false, false, false}

	const goroutines = 64
	const iterations = 200

	runPhase := func(spec Spec, distinct *[]Predicate, mu *sync.Mutex) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errCh := make(chan string, goroutines)

		for worker := 0; worker < goroutines; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-start
				for iteration := 0; iteration < iterations; iteration++ {
					if worker%2 == 0 {
						// Concurrent first compiles and cache hits must all
						// converge on the single cached predicate.
						predicate, predicateDiags := compiler.Compile(spec)
						if predicate == nil || predicateDiags.HasErrors() {
							select {
							case errCh <- fmt.Sprintf("unexpected compile failure for %s", spec.Version):
							default:
							}
							return
						}
						mu.Lock()
						*distinct = append(*distinct, predicate)
						mu.Unlock()
					} else {
						// Matching on a shared predicate must stay consistent
						// while other goroutines churn the compiler.
						reference, _ := compiler.Compile(spec)
						for index, candidate := range views {
							got := reference.Matches(candidate)
							if spec.Version == "v1" && got != expectedV1[index] {
								select {
								case errCh <- fmt.Sprintf("worker %d: inconsistent v1 match result", worker):
								default:
								}
								return
							}
						}
					}
				}
			}(worker)
		}

		close(start)
		wg.Wait()
		close(errCh)
		for message := range errCh {
			t.Fatal(message)
		}
	}

	// Phase 1: simultaneous first-time compiles of v1 converge on one entry.
	var v1Predicates []Predicate
	runPhase(specV1, &v1Predicates, &sync.Mutex{})
	v1Count := map[Predicate]int{}
	for _, predicate := range v1Predicates {
		v1Count[predicate]++
	}
	require.Len(t, v1Count, 1, "all v1 compiles must return the same cached predicate")
	var v1 Predicate
	for predicate := range v1Count {
		v1 = predicate
	}

	// Version flip atomically replaces the cached entry.
	v2, v2Diags := compiler.Compile(specV2)
	require.NotNil(t, v2)
	assert.False(t, v2Diags.HasErrors())
	assert.NotSame(t, v1, v2)

	// Phase 2: v2 compiles converge on the new entry while the old immutable
	// predicate remains valid and usable.
	var v2Predicates []Predicate
	runPhase(specV2, &v2Predicates, &sync.Mutex{})
	v2Count := map[Predicate]int{}
	for _, predicate := range v2Predicates {
		v2Count[predicate]++
	}
	require.Len(t, v2Count, 1)
	for predicate := range v2Count {
		assert.Same(t, v2, predicate)
	}

	assert.True(t, v1.Matches(view(map[string]string{"env": "prod", "tier": "web"}, nil)))
	assert.True(t, v2.Matches(view(
		map[string]string{"env": "prod", "tier": "web", "team": "core"},
		nil,
	)))
	assert.False(t, v2.Matches(view(
		map[string]string{"env": "prod", "tier": "web", "team": "external"},
		nil,
	)))
}

func TestDiagnosticErrorShape(t *testing.T) {
	diagnostic := errorDiagnostic("labelSelector.requirements[0].operator", "illegal operator")
	assert.Equal(t, "labelSelector.requirements[0].operator: illegal operator", diagnostic.Error())

	collection := Diagnostics{
		errorDiagnostic("a", "first"),
		warningDiagnostic("b", "second"),
	}
	assert.True(t, collection.HasErrors())
	assert.Contains(t, collection.Error(), "a: first")
	assert.Contains(t, collection.Error(), "b: second")
}
