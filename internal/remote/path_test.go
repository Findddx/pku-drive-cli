package remote

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/", "/", false},
		{"//个人文档///CLI测试/.", "/个人文档/CLI测试", false},
		{"个人文档/file", "", true},
		{"/个人文档/../部门文档", "", true},
		{"/个人文档/\x00bad", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
