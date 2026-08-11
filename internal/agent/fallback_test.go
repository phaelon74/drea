package agent

import "testing"

func TestParseToolCallFromText(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		want    string
		wantArg string
	}{
		{
			name:    "object arguments",
			in:      `{"name":"list_dir","arguments":{"path":"."}}`,
			wantOK:  true,
			want:    "list_dir",
			wantArg: `{"path":"."}`,
		},
		{
			name:    "string arguments",
			in:      `{"name":"read_file","arguments":"{\"path\":\"x\"}"}`,
			wantOK:  true,
			want:    "read_file",
			wantArg: `{"path":"x"}`,
		},
		{
			name:    "tool key with inline fields",
			in:      `{"tool":"list_dir","path":"."}`,
			wantOK:  true,
			want:    "list_dir",
			wantArg: `{"path":"."}`,
		},
		{
			name:    "code fenced",
			in:      "```json\n{\"name\":\"search\",\"arguments\":{\"pattern\":\"x\"}}\n```",
			wantOK:  true,
			want:    "search",
			wantArg: `{"pattern":"x"}`,
		},
		{
			name:   "plain prose",
			in:     "I will list the directory now.",
			wantOK: false,
		},
		{
			name:   "not json",
			in:     "list_dir .",
			wantOK: false,
		},
		{
			name:   "json without a tool name",
			in:     `{"foo":"bar"}`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, ok := parseToolCallsFromText(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			tc := calls[0]
			if tc.Function.Name != c.want {
				t.Errorf("name = %q, want %q", tc.Function.Name, c.want)
			}
			if tc.Function.Arguments != c.wantArg {
				t.Errorf("arguments = %q, want %q", tc.Function.Arguments, c.wantArg)
			}
		})
	}
}
