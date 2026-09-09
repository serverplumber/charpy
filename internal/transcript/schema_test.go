package transcript_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../../schema/transcript/v1.json"

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	f, err := os.Open(schemaPath)
	if err != nil {
		t.Fatalf("open transcript schema: %v", err)
	}
	defer f.Close()

	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("transcript schema is not valid JSON: %v", err)
	}

	c := jsonschema.NewCompiler()
	const url = "https://charpy.dev/schema/transcript/v1.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("transcript schema does not compile: %v", err)
	}
	return sch
}

// The schema must itself be well-formed before it can be trusted to reject
// anything.
func TestSchemaCompiles(t *testing.T) {
	compileSchema(t)
}

// Every golden transcript validates line by line. These transcripts are
// written by hand precisely so they are not co-generated with the code that
// reads them.
func TestGoldenTranscriptsValidate(t *testing.T) {
	sch := compileSchema(t)

	paths, err := filepath.Glob("../../testdata/transcripts/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no golden transcripts found")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			for i, line := range readLines(t, path) {
				lineNo := i + 1
				v, err := jsonschema.UnmarshalJSON(strings.NewReader(line))
				if err != nil {
					t.Fatalf("line %d is not valid JSON: %v", lineNo, err)
				}
				if err := sch.Validate(v); err != nil {
					t.Errorf("line %d does not validate:\n%v", lineNo, err)
				}
			}
		})
	}
}

// Guarantee 1 of docs/design/transcript.md: file order equals seq order, and
// seq is dense from 0 with no gaps. The oracle relies on this.
func TestSequenceIsDenseAndOrdered(t *testing.T) {
	paths, _ := filepath.Glob("../../testdata/transcripts/*.jsonl")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			for i, line := range readLines(t, path) {
				var probe struct {
					Seq  int    `json:"seq"`
					Type string `json:"type"`
				}
				if err := json.Unmarshal([]byte(line), &probe); err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				if probe.Seq != i {
					t.Errorf("line %d has seq %d, want %d (dense, in order)", i+1, probe.Seq, i)
				}
				if i == 0 && probe.Type != "header" {
					t.Errorf("first line must be the header, got type %q", probe.Type)
				}
				if i > 0 && probe.Type == "header" {
					t.Errorf("line %d is a second header; exactly one is allowed", i+1)
				}
			}
		})
	}
}

// A malformed frame keeps its bytes and drops its parsed envelope. charpy
// deliberately emits frames no parser accepts, so this shape has to survive.
func TestMalformedFrameKeepsRawBytes(t *testing.T) {
	sch := compileSchema(t)

	const malformed = `{"schema_version":1,"type":"frame","run_id":"r","seq":1,
	  "t_mono_ns":1,"t_wall":"2026-09-08T14:03:10Z","face":"downstream",
	  "direction":"s2c","transport":"stdio","client_id":"c0","session_id":"s1",
	  "kind":"malformed","id":null,
	  "id_type":"absent","method":null,"result_type":null,"error_code":null,
	  "raw":"eyJqc29u","raw_len":8,"raw_truncated":false,"http":null,
	  "link":{"via":"none","confidence":0.0}}`

	v, err := jsonschema.UnmarshalJSON(strings.NewReader(malformed))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("a malformed frame must validate:\n%v", err)
	}
}

// The schema has to reject as well as accept, or it is decoration.
func TestSchemaRejectsMalformedLines(t *testing.T) {
	sch := compileSchema(t)

	tests := []struct {
		name string
		line string
		why  string
	}{
		{
			name: "malformed frame claiming a method",
			why:  "a frame no parser accepts cannot have a parsed method",
			line: `{"schema_version":1,"type":"frame","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","face":"downstream","direction":"s2c",
			  "transport":"stdio","client_id":"c0","session_id":"s1","kind":"malformed","id":null,"id_type":"absent",
			  "method":"tools/call","raw":"eyJ4IjoxfQ==","raw_len":8,"raw_truncated":false,
			  "http":null,"link":{"via":"none","confidence":0.0}}`,
		},
		{
			name: "inferred join claiming full confidence",
			why:  "only forwarded and traced joins are authoritative",
			line: `{"schema_version":1,"type":"frame","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","face":"upstream","direction":"c2s",
			  "transport":"http","client_id":"c0","session_id":"s1",
			  "kind":"request","id":"1","id_type":"number",
			  "method":"tools/list","raw":"eyJ4IjoxfQ==","raw_len":8,"raw_truncated":false,
			  "link":{"charpy_id":"x","via":"inferred","confidence":1.0}}`,
		},
		{
			name: "stream_close without a reason",
			why:  "the cancellation invariant needs to know who closed the stream",
			line: `{"schema_version":1,"type":"event","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","event_kind":"stream_close",
			  "detail":{"bytes_written":10}}`,
		},
		{
			name: "probe without an outcome",
			why:  "liveness is computed from probe outcomes",
			line: `{"schema_version":1,"type":"event","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","event_kind":"probe","detail":{"method":"ping"}}`,
		},
		{
			name: "http populated on a stdio frame",
			why:  "stdio has no headers or status codes",
			line: `{"schema_version":1,"type":"frame","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","face":"downstream","direction":"c2s",
			  "transport":"stdio","client_id":"c0","session_id":"s1",
			  "kind":"request","id":"1","id_type":"number",
			  "method":"tools/list","raw":"eyJ4IjoxfQ==","raw_len":8,"raw_truncated":false,
			  "http":{"status":200},"link":{"via":"none","confidence":0.0}}`,
		},
		{
			name: "id carried as a number",
			why:  "id is always canonical JSON text so the column has one type",
			line: `{"schema_version":1,"type":"frame","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","face":"downstream","direction":"c2s",
			  "transport":"stdio","client_id":"c0","session_id":"s1",
			  "kind":"request","id":7,"id_type":"number",
			  "method":"tools/list","raw":"eyJ4IjoxfQ==","raw_len":8,"raw_truncated":false,
			  "http":null,"link":{"via":"none","confidence":0.0}}`,
		},
		{
			name: "frame without client_id and session_id",
			why:  "dimension totality: v0 emits constants, but never omits them",
			line: `{"schema_version":1,"type":"frame","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","face":"downstream","direction":"c2s",
			  "transport":"stdio","kind":"request","id":"1","id_type":"number",
			  "method":"tools/list","raw":"eyJ4IjoxfQ==","raw_len":8,"raw_truncated":false,
			  "http":null,"link":{"via":"none","confidence":0.0}}`,
		},
		{
			name: "unknown top level field",
			why:  "additionalProperties is false so typos fail loudly",
			line: `{"schema_version":1,"type":"event","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","event_kind":"note","nonsense":true}`,
		},
		{
			name: "future schema version",
			why:  "a v1 reader must refuse rather than guess",
			line: `{"schema_version":2,"type":"event","run_id":"r","seq":1,"t_mono_ns":1,
			  "t_wall":"2026-09-08T14:03:10Z","event_kind":"note"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.line))
			if err != nil {
				t.Fatalf("test line is not valid JSON: %v", err)
			}
			if err := sch.Validate(v); err == nil {
				t.Errorf("schema accepted an invalid line; %s", tc.why)
			}
		})
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}
