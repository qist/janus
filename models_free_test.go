package main

import (
	"encoding/json"
	"testing"
)

func modelFromJSON(t *testing.T, cost string) OCModel {
	t.Helper()
	var m OCModel
	if err := json.Unmarshal([]byte(`{"cost":`+cost+`}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestIsFreeModel(t *testing.T) {
	cases := []struct {
		name string
		cost string
		want bool
	}{
		{"零价单档", `[{"input":0,"output":0}]`, true},
		{"零价多档", `[{"input":0,"output":0},{"input":0,"output":0}]`, true},
		{"收费单档", `[{"input":2,"output":10}]`, false},
		{"混合档有一档收费", `[{"input":0,"output":0},{"input":0.1,"output":0}]`, false},
		{"无价格信息", `[]`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isFreeModel(modelFromJSON(t, c.cost)); got != c.want {
				t.Errorf("isFreeModel(%s) = %v, want %v", c.cost, got, c.want)
			}
		})
	}
}

func TestToOpenAIMarksFree(t *testing.T) {
	m := modelFromJSON(t, `[{"input":0,"output":0}]`)
	m.ProviderID = "opencode"
	m.ID = "x-free"
	if o := ToOpenAI(m); !o.Free || o.ID != "opencode/x-free" {
		t.Fatalf("期望 free 标记, got %+v", o)
	}
	paid := modelFromJSON(t, `[{"input":1,"output":2}]`)
	paid.ProviderID = "opencode-go"
	paid.ID = "y"
	if o := ToOpenAI(paid); o.Free {
		t.Fatalf("收费模型不该标 free: %+v", o)
	}
}
