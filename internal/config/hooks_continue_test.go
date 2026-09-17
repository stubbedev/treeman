package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestActionContinueOnError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want, bad  bool
	}{
		{name: "default", body: "run: npm ci"},
		{name: "explicit false", body: "run: npm ci\ncontinue_on_error: false"},
		{name: "opt in", body: "run: [npm ci, npm run build]\ncontinue_on_error: true", want: true},
		{name: "invalid", body: "run: npm ci\ncontinue_on_error: sometimes", bad: true},
		{name: "mapping", body: "run: npm ci\ncontinue_on_error: {}", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Action
			err := yaml.Unmarshal([]byte(tc.body), &a)
			if (err != nil) != tc.bad {
				t.Fatalf("decode error = %v, want error %v", err, tc.bad)
			}
			if tc.bad {
				return
			}
			if a.ContinueOnError != tc.want {
				t.Fatalf("ContinueOnError = %v", a.ContinueOnError)
			}
			body, err := yaml.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip Action
			if err := yaml.Unmarshal(body, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if roundTrip.ContinueOnError != tc.want {
				t.Fatal("policy lost during round trip")
			}
		})
	}
	var filtered FilteredAction
	if err := yaml.Unmarshal([]byte("match: frontend\nrun: npm ci\ncontinue_on_error: true"), &filtered); err != nil {
		t.Fatal(err)
	}
	if !filtered.ContinueOnError || len(filtered.Match) != 1 {
		t.Fatalf("filtered action: %+v", filtered)
	}
	for _, schema := range []struct {
		name         string
		actionSchema bool
	}{{"action", true}, {"filtered", false}} {
		s := Action{}.JSONSchema()
		if !schema.actionSchema {
			s = FilteredAction{}.JSONSchema()
		}
		p, ok := s.Properties.Get("continue_on_error")
		if !ok || p.Type != "boolean" || p.Default != false {
			t.Fatalf("%s policy schema: %+v", schema.name, p)
		}
	}
}
