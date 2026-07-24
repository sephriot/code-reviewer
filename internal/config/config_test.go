package config

import "testing"

func TestParseAgentArgv(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "valid command with force",
			input: `["agent","--print","--output-format","json","--trust","--force"]`,
			want:  []string{"agent", "--print", "--output-format", "json", "--trust", "--force"},
		},
		{
			name:    "invalid json",
			input:   "not-json",
			wantErr: true,
		},
		{
			name:    "empty array",
			input:   `[]`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAgentArgv(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseAgentArgv() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(got) != len(tt.want) {
				t.Fatalf("parseAgentArgv() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("parseAgentArgv() = %#v, want %#v", got, tt.want)
				}
			}
		})
	}
}
