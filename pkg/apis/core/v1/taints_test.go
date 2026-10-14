package v1

import "testing"

func TestTolerations(t *testing.T) {
	cp := Taint{Key: TaintNodeRoleControlPlane, Effect: TaintEffectNoSchedule}
	gpu := Taint{Key: "gpu", Value: "a100", Effect: TaintEffectNoExecute}
	for name, tc := range map[string]struct {
		toleration Toleration
		taint      Taint
		want       bool
	}{
		"exists by key":                {Toleration{Key: TaintNodeRoleControlPlane, Operator: TolerationOpExists}, cp, true},
		"exists with effect":           {Toleration{Key: TaintNodeRoleControlPlane, Operator: TolerationOpExists, Effect: TaintEffectNoSchedule}, cp, true},
		"other effect":                 {Toleration{Key: TaintNodeRoleControlPlane, Operator: TolerationOpExists, Effect: TaintEffectNoExecute}, cp, false},
		"empty key exists matches all": {Toleration{Operator: TolerationOpExists}, gpu, true},
		"equal value":                  {Toleration{Key: "gpu", Value: "a100"}, gpu, true},
		"equal other value":            {Toleration{Key: "gpu", Operator: TolerationOpEqual, Value: "t4"}, gpu, false},
		"other key":                    {Toleration{Key: "gpu", Operator: TolerationOpExists}, cp, false},
	} {
		if got := tc.toleration.ToleratesTaint(tc.taint); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	soft := Taint{Key: "x", Effect: TaintEffectPreferNoSchedule}
	if taint, ok := UntoleratedTaint([]Taint{soft, cp}, nil); !ok || taint.Key != TaintNodeRoleControlPlane || taint.String() != "{node-role.kubernetes.io/control-plane: }" {
		t.Fatalf("untolerated: %v %v", taint, ok)
	}
	if _, ok := UntoleratedTaint([]Taint{soft, cp}, []Toleration{{Key: TaintNodeRoleControlPlane, Operator: TolerationOpExists}}); ok {
		t.Fatal("PreferNoSchedule does not block scheduling")
	}
}
