package control

import "testing"

func TestChildRole(t *testing.T) {
	tests := []struct {
		name      string
		parent    Role
		requested Role
		want      Role
		wantError bool
	}{
		{"controller requires explicit role", RoleController, "", "", true},
		{"controller cannot create worker", RoleController, RoleWorker, "", true},
		{"controller cannot clone", RoleController, RoleController, "", true},
		{"manager requires explicit role", RoleManager, "", "", true},
		{"manager cannot create manager", RoleManager, RoleManager, "", true},
		{"worker cannot spawn", RoleWorker, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (Requester{Role: tt.parent}).ChildRole(tt.requested)
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("ChildRole() = (%q, %v), want (%q, error=%v)", got, err, tt.want, tt.wantError)
			}
		})
	}
}
