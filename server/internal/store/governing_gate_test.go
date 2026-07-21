package store

import "testing"

func TestGoverningGateValidate(t *testing.T) {
	tests := []struct {
		name    string
		gate    *GoverningGate
		wantErr bool
	}{
		{"nil is valid (no gate)", nil, false},
		{"required 1 ok", &GoverningGate{Required: 1}, false},
		{"required 0 rejected", &GoverningGate{Required: 0}, true},
		{"required negative rejected", &GoverningGate{Required: -1}, true},
		{"required over cap rejected", &GoverningGate{Required: maxGateRequired + 1}, true},
		{"too many approvers rejected", &GoverningGate{Required: 1, Approvers: make([]string, maxGateApprovers+1)}, true},
		{"too many groups rejected", &GoverningGate{Required: 1, ApproverGroups: make([]string, maxGateGroups+1)}, true},
		{"full valid gate", &GoverningGate{Required: 2, Approvers: []string{"a"}, ApproverGroups: []string{"g"}, Description: "d"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.gate.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGoverningGateMarshalRoundtrip(t *testing.T) {
	// nil <-> nil bytes (SQL NULL).
	b, err := marshalGoverningGate(nil)
	if err != nil || b != nil {
		t.Fatalf("marshal(nil) = %v, %v; want nil, nil", b, err)
	}
	if g, err := unmarshalGoverningGate(nil); err != nil || g != nil {
		t.Fatalf("unmarshal(nil) = %v, %v; want nil, nil", g, err)
	}
	// A JSON `null` also decodes to nil (not a zero-value gate).
	if g, err := unmarshalGoverningGate([]byte("null")); err != nil || g != nil {
		t.Fatalf("unmarshal(null) = %v, %v; want nil, nil", g, err)
	}

	in := &GoverningGate{Approvers: []string{"a@x"}, ApproverGroups: []string{"sre"}, Required: 3, Description: "d"}
	b, err = marshalGoverningGate(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := unmarshalGoverningGate(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !GoverningGateEqual(in, out) {
		t.Errorf("roundtrip = %+v, want %+v", out, in)
	}
}

func TestGoverningGateEqual(t *testing.T) {
	base := &GoverningGate{Approvers: []string{"a", "b"}, ApproverGroups: []string{"g"}, Required: 2, Description: "d"}
	tests := []struct {
		name string
		a, b *GoverningGate
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs set", nil, base, false},
		{"identical", base, &GoverningGate{Approvers: []string{"a", "b"}, ApproverGroups: []string{"g"}, Required: 2, Description: "d"}, true},
		{"different required", base, &GoverningGate{Approvers: []string{"a", "b"}, ApproverGroups: []string{"g"}, Required: 3, Description: "d"}, false},
		{"different description", base, &GoverningGate{Approvers: []string{"a", "b"}, ApproverGroups: []string{"g"}, Required: 2, Description: "e"}, false},
		{"reordered approvers => change (order-sensitive, fail-closed)", base, &GoverningGate{Approvers: []string{"b", "a"}, ApproverGroups: []string{"g"}, Required: 2, Description: "d"}, false},
		{"extra approver", base, &GoverningGate{Approvers: []string{"a", "b", "c"}, ApproverGroups: []string{"g"}, Required: 2, Description: "d"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GoverningGateEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("GoverningGateEqual = %v, want %v", got, tt.want)
			}
		})
	}
}
