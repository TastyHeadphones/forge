package release

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A shape that opts out of nothing encodes without the field, so every shape
// recorded before it existed hashes and diffs exactly as it did.
func TestShape_ObservabilityPolicyOmittedWhenAbsent(t *testing.T) {
	b, err := Shape{Kind: EnvLocal}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "observability_policy") {
		t.Errorf("an absent policy must not be encoded: %s", b)
	}
}

func TestShape_ObservabilityPolicyRoundTrip(t *testing.T) {
	in := Shape{Kind: EnvSelfManaged, ObservabilityPolicy: &ShapeObservabilityPolicy{
		Enabled: true, Disabled: []string{ObsCategoryMetadata, ObsCategoryLogs},
	}}
	b, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"observability_policy"`, `"enabled":true`, `"disabled":["logs","metadata"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("encoded shape lacks %s: %s", want, b)
		}
	}
	var out Shape
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("a round-tripped shape is invalid: %v", err)
	}
	if want := in.Canonical(); !reflect.DeepEqual(out.ObservabilityPolicy, want.ObservabilityPolicy) {
		t.Errorf("round trip changed the policy:\n got %+v\nwant %+v", out.ObservabilityPolicy, want.ObservabilityPolicy)
	}
}

func TestShape_ObservabilityPolicyAllOff(t *testing.T) {
	in := Shape{Kind: EnvSelfManaged, ObservabilityPolicy: &ShapeObservabilityPolicy{Enabled: false, Disabled: ObsCategories}}
	if err := in.Canonical().Validate(); err != nil {
		t.Fatalf("a fully opted-out policy is valid: %v", err)
	}
	for _, c := range ObsCategories {
		if !in.ObservabilityPolicy.Off(c) {
			t.Errorf("Off(%q) = false under enabled=false", c)
		}
	}
}

func TestShape_ObservabilityPolicyOptIn(t *testing.T) {
	in := Shape{Kind: EnvSelfManaged, ObservabilityPolicy: &ShapeObservabilityPolicy{Enabled: true, OptIn: []string{ObsOptInReplay}}}
	b, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"opt_in":["replay"]`) {
		t.Errorf("opt_in not encoded: %s", b)
	}
	if !in.ObservabilityPolicy.OptedIn(ObsOptInReplay) || in.ObservabilityPolicy.OptedIn("nonsense") {
		t.Error("OptedIn wrong")
	}
	// An env that disabled everything is never opted in.
	off := &ShapeObservabilityPolicy{Enabled: false, Disabled: ObsCategories, OptIn: []string{ObsOptInReplay}}
	if off.OptedIn(ObsOptInReplay) {
		t.Error("a fully disabled env must not count as opted in")
	}
	for name, p := range map[string]*ShapeObservabilityPolicy{
		"unknown opt-in": {Enabled: true, OptIn: []string{"audio"}},
		"duplicate":      {Enabled: true, OptIn: []string{"replay", "replay"}},
	} {
		if err := (Shape{Kind: EnvLocal, ObservabilityPolicy: p}).Canonical().Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestShape_ObservabilityPolicyRefusals(t *testing.T) {
	for name, p := range map[string]*ShapeObservabilityPolicy{
		"unknown category":       {Enabled: true, Disabled: []string{"infra"}},
		"duplicate":              {Enabled: true, Disabled: []string{"logs", "logs"}},
		"off without categories": {Enabled: false},
	} {
		err := Shape{Kind: EnvLocal, ObservabilityPolicy: p}.Canonical().Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestShapeObservabilityPolicy_Off(t *testing.T) {
	var none *ShapeObservabilityPolicy
	if none.Off(ObsCategoryLogs) {
		t.Error("a nil policy opts out of nothing")
	}
	p := &ShapeObservabilityPolicy{Enabled: true, Disabled: []string{ObsCategoryLogs}}
	if !p.Off(ObsCategoryLogs) || p.Off(ObsCategoryTraces) {
		t.Errorf("Off wrong for %+v", p)
	}
}
