package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmitJSONReport_StdoutPreservesHistoricalShape(t *testing.T) {
	var stdout, stderr bytes.Buffer
	emitJSONReport(&stdout, &stderr, []byte(`{"phase":"ci","pass_fail":"pass"}`))

	got := stdout.String()
	want := "{\"phase\":\"ci\",\"pass_fail\":\"pass\"}\n"
	if got != want {
		t.Fatalf("stdout shape changed:\n got=%q\nwant=%q", got, want)
	}
}

func TestEmitJSONReport_StderrWrapsWithSentinels(t *testing.T) {
	var stdout, stderr bytes.Buffer
	body := []byte(`{"phase":"ci"}`)
	emitJSONReport(&stdout, &stderr, body)

	out := stderr.String()
	if !strings.Contains(out, ReportSentinelBegin) {
		t.Errorf("stderr missing begin sentinel: %q", out)
	}
	if !strings.Contains(out, ReportSentinelEnd) {
		t.Errorf("stderr missing end sentinel: %q", out)
	}
	extracted, ok := ExtractReportFromMixedStream([]byte(out))
	if !ok || extracted != string(body) {
		t.Errorf("extract failed: ok=%v extracted=%q", ok, extracted)
	}
}

func TestExtractReportFromMixedStream_PicksLastBlock(t *testing.T) {
	mixed := []byte(
		"some build output\n" +
			ReportSentinelBegin + "\n" +
			"{\"stale\":true}\n" +
			ReportSentinelEnd + "\n" +
			"trailing build output\n" +
			ReportSentinelBegin + "\n" +
			"{\"final\":true}\n" +
			ReportSentinelEnd + "\n",
	)
	got, ok := ExtractReportFromMixedStream(mixed)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != `{"final":true}` {
		t.Errorf("got %q, want last block", got)
	}
}

func TestExtractReportFromMixedStream_MissingMarkers(t *testing.T) {
	if _, ok := ExtractReportFromMixedStream([]byte("nothing here")); ok {
		t.Error("expected ok=false on missing markers")
	}
	// Unbalanced (end without begin).
	if _, ok := ExtractReportFromMixedStream([]byte("foo " + ReportSentinelEnd)); ok {
		t.Error("expected ok=false when only the end marker is present")
	}
}

func TestEmitJSONReport_NilSinks(t *testing.T) {
	// Must not panic on nil sinks.
	emitJSONReport(nil, nil, []byte(`{}`))
}
