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

func TestAddDefaultTolerations(t *testing.T) {
	spec := &PodSpec{}
	AddDefaultTolerations(spec)
	if len(spec.Tolerations) != 2 || spec.Tolerations[0].Key != TaintNodeNotReady || *spec.Tolerations[1].TolerationSeconds != 300 ||
		spec.Tolerations[1].Effect != TaintEffectNoExecute || spec.Tolerations[1].Operator != TolerationOpExists {
		t.Fatalf("%+v", spec.Tolerations)
	}
	AddDefaultTolerations(spec)
	if len(spec.Tolerations) != 2 {
		t.Fatal("adding twice changes nothing")
	}
	short := int64(5)
	own := &PodSpec{Tolerations: []Toleration{{Key: TaintNodeUnreachable, Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &short}}}
	AddDefaultTolerations(own)
	if len(own.Tolerations) != 2 || *own.Tolerations[0].TolerationSeconds != 5 {
		t.Fatalf("a Pod's own toleration wins: %+v", own.Tolerations)
	}
	all := &PodSpec{Tolerations: []Toleration{{Operator: TolerationOpExists}}}
	if AddDefaultTolerations(all); len(all.Tolerations) != 1 {
		t.Fatalf("tolerating everything needs nothing more: %+v", all.Tolerations)
	}
}

func TestMatchesNodeSelector(t *testing.T) {
	node := &Node{}
	node.Name = "worker-1"
	node.Labels = map[string]string{"disk": "ssd", "cores": "8"}
	req := func(key, op string, values ...string) NodeSelectorRequirement {
		return NodeSelectorRequirement{Key: key, Operator: op, Values: values}
	}
	affinity := func(terms ...NodeSelectorTerm) *PodSpec {
		return &PodSpec{Affinity: &Affinity{NodeAffinity: &NodeAffinity{Required: &NodeSelector{NodeSelectorTerms: terms}}}}
	}
	for name, tc := range map[string]struct {
		spec *PodSpec
		want bool
	}{
		"no constraints":              {&PodSpec{}, true},
		"nodeSelector":                {&PodSpec{NodeSelector: map[string]string{"disk": "ssd"}}, true},
		"nodeSelector misses":         {&PodSpec{NodeSelector: map[string]string{"disk": "hdd"}}, false},
		"empty value needs the label": {&PodSpec{NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, false},
		"In":                          {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("disk", "In", "nvme", "ssd")}}), true},
		"NotIn":                       {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("disk", "NotIn", "ssd")}}), false},
		"Exists and Gt":               {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("disk", "Exists"), req("cores", "Gt", "4")}}), true},
		"Lt fails":                    {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("cores", "Lt", "4")}}), false},
		"DoesNotExist":                {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("gpu", "DoesNotExist")}}), true},
		"terms are ORed":              {affinity(NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{req("gpu", "Exists")}}, NodeSelectorTerm{MatchFields: []NodeSelectorRequirement{req("metadata.name", "In", "worker-1")}}), true},
		"empty term matches 0":        {affinity(NodeSelectorTerm{}), false},
	} {
		if got := MatchesNodeSelector(tc.spec, node); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}
