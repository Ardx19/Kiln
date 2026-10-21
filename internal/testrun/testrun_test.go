package testrun

import (
	"reflect"
	"testing"
)

const ctestXML = `<?xml version="1.0"?>
<testsuite name="proj" tests="4" failures="1">
  <testcase name="pass" classname="pass" time="0.1"/>
  <testcase name="fail" classname="fail"><failure message="Failed"/></testcase>
  <testcase name="slow" classname="slow"><error message="Timeout"/></testcase>
  <testcase name="skipme" classname="skipme"><skipped/></testcase>
</testsuite>`

const nestedXML = `<testsuites><testsuite name="a"><testcase name="t1"/><testsuite name="inner"><testcase name="t2"><failure message="x"/></testcase></testsuite></testsuite></testsuites>`

func TestParseJUnit(t *testing.T) {
	rs, err := ParseJUnit([]byte(ctestXML))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rs {
		got[r.Name] = r.Status
	}
	want := map[string]string{"pass": "pass", "fail": "fail", "slow": "fail", "skipme": "skip"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	rs, err = ParseJUnit([]byte(nestedXML))
	if err != nil || len(rs) != 2 || rs[1].Name != "t2" || rs[1].Status != "fail" {
		t.Errorf("nested: %+v, %v", rs, err)
	}
	if _, err := ParseJUnit([]byte("not xml")); err == nil {
		t.Error("garbage should be an error")
	}
}

func TestExpectedAndJudge(t *testing.T) {
	exp, err := ParseExpected("# flaky tests\nfail  # needs a display\n/^sl/ timing dependent\n\nwasbroken known bug\n")
	if err != nil {
		t.Fatal(err)
	}
	if why, ok := exp.Match("fail"); !ok || why != "needs a display" {
		t.Errorf("Match(fail) = %q, %v", why, ok)
	}
	if why, ok := exp.Match("slow"); !ok || why != "timing dependent" {
		t.Errorf("Match(slow) = %q, %v", why, ok)
	}
	rs, _ := ParseJUnit([]byte(ctestXML))
	rs = append(rs, Result{Name: "wasbroken", Status: "pass"})
	v := Judge(rs, exp)
	if !v.OK(false) || v.OK(true) {
		t.Errorf("verdict %+v: only expected failures failed (ok), but wasbroken passed (strict fails)", v)
	}
	if v.Passed != 1 || len(v.XFail) != 2 || len(v.XPass) != 1 || v.Skipped != 1 || len(v.Failed) != 0 {
		t.Errorf("counts: %+v", v)
	}
	if got := v.Summary(); got != "2 passed, 0 failed, 2 expected failures, 1 unexpectedly passed, 1 skipped" {
		t.Errorf("summary = %q", got)
	}
	v = Judge(rs, &Expected{exact: map[string]string{}})
	if v.OK(false) || !reflect.DeepEqual(Names(v.Failed), []string{"fail", "slow"}) {
		t.Errorf("without a list: %+v", v)
	}
	if _, err := ParseExpected("/(unclosed/"); err == nil {
		t.Error("a bad regex should be an error")
	}
}
