package upstream

import "testing"

func TestNewAPIGroupsPreservesAutoGroupWithoutNumericRatio(t *testing.T) {
	groups := newAPIGroups(
		map[string]any{},
		map[string]any{"usable_group": map[string]any{"auto": "automatic routing"}},
	)
	if len(groups) != 1 {
		t.Fatalf("expected auto group to remain visible, got %#v", groups)
	}
	if groups[0].Name != "auto" || groups[0].Multiplier != nil || groups[0].MultiplierMode != "auto" {
		t.Fatalf("unexpected auto group normalization: %#v", groups[0])
	}
}

// TestNewAPIGroupsPrefersSelfGroupsOverPricingGroupRatio covers the ruoli gpt-pro case:
// /api/pricing group_ratio is the global GroupRatio (0.3), while /api/user/self/groups
// returns the effective ratio after GroupGroupRatio (0.15). Self must win so the
// admin group-rates page matches the upstream token-creation UI.
func TestNewAPIGroupsPrefersSelfGroupsOverPricingGroupRatio(t *testing.T) {
	groups := newAPIGroups(
		map[string]any{
			"data": map[string]any{
				"gpt-pro": map[string]any{"ratio": 0.15, "desc": "gpt pro 反代 codex, 稳定"},
				"gork":    map[string]any{"ratio": 0.1, "desc": "gork 反代"},
				"vip":     map[string]any{"ratio": 1.0, "desc": "用户分组"},
			},
		},
		map[string]any{
			"data": map[string]any{
				"group_ratio": map[string]any{
					"gpt-pro": 0.3,
					"gork":    0.1,
					"vip":     1.0,
					"cn":      0.8, // only in pricing — still included as fallback
				},
				"usable_group": map[string]any{
					"gpt-pro": "gpt pro",
					"gork":    "gork",
					"vip":     "vip",
					"cn":      "cn",
				},
			},
		},
	)

	byName := map[string]GroupInfo{}
	for _, g := range groups {
		byName[g.Name] = g
	}

	if g := byName["gpt-pro"]; g.Multiplier == nil || *g.Multiplier != 0.15 {
		t.Fatalf("gpt-pro multiplier = %v, want 0.15 from self/groups (not pricing 0.3)", g.Multiplier)
	}
	if g := byName["gork"]; g.Multiplier == nil || *g.Multiplier != 0.1 {
		t.Fatalf("gork multiplier = %v, want 0.1", g.Multiplier)
	}
	if g := byName["cn"]; g.Multiplier == nil || *g.Multiplier != 0.8 {
		t.Fatalf("cn (pricing-only) multiplier = %v, want 0.8 fallback", g.Multiplier)
	}
	if _, ok := byName["vip"]; !ok {
		t.Fatalf("vip missing from groups: %#v", groups)
	}
}

func TestNewAPIGroupsPricingOnlyWhenSelfGroupsEmpty(t *testing.T) {
	groups := newAPIGroups(
		map[string]any{},
		map[string]any{
			"group_ratio": map[string]any{"default": 1.0, "vip": 0.5},
		},
	)
	byName := map[string]GroupInfo{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	if g := byName["vip"]; g.Multiplier == nil || *g.Multiplier != 0.5 {
		t.Fatalf("pricing-only vip = %v, want 0.5", g.Multiplier)
	}
	if g := byName["default"]; g.Multiplier == nil || *g.Multiplier != 1.0 {
		t.Fatalf("pricing-only default = %v, want 1.0", g.Multiplier)
	}
}
