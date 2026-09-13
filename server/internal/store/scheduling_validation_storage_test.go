package store

import "testing"

func TestValidateStorageQuantity(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty is allowed (not set)", "", false},
		{"valid Gi", "50Gi", false},
		{"valid large", "300Gi", false},
		{"valid Mi", "512Mi", false},
		{"zero rejected", "0", true},
		{"negative rejected", "-1Gi", true},
		{"garbage rejected", "big", true},
		{"unit typo rejected", "50GB", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStorageQuantity("dind_storage_size", tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateStorageQuantity(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateStorageClassName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty is allowed (cluster default)", "", false},
		{"simple", "premium-rwo", false},
		{"local-ssd", "local-ssd", false},
		{"dotted subdomain", "csi.example.com", false},
		{"uppercase rejected", "Premium", true},
		{"space rejected", "premium rwo", true},
		{"leading dash rejected", "-bad", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStorageClassName("dind_storage_class", tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateStorageClassName(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
		})
	}
}

// applySchedulingValidation must reject bad storage input so neither the
// admin API nor the YAML seed can persist a size that fails at PVC admission.
func TestApplySchedulingValidation_RejectsBadStorage(t *testing.T) {
	if err := applySchedulingValidation(&RunnerProfileInput{DinDStorageSize: "nope"}); err == nil {
		t.Error("expected error for invalid dind_storage_size, got nil")
	}
	if err := applySchedulingValidation(&RunnerProfileInput{WorkspaceStorageClass: "Bad Class"}); err == nil {
		t.Error("expected error for invalid workspace_storage_class, got nil")
	}
	if err := applySchedulingValidation(&RunnerProfileInput{
		WorkspaceSize: "200Gi", DinDStorageSize: "300Gi", DinDStorageClass: "premium-rwo",
	}); err != nil {
		t.Errorf("valid storage input rejected: %v", err)
	}
}
