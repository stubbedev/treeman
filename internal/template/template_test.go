package template

import (
	"errors"
	"testing"

	"github.com/stubbedev/treeman/internal/slug"
)

func ctx() Context {
	return FromSlug(slug.Slug{Value: "proj_1234", Source: slug.SourceTicket})
}

func TestRendersKnownKeys(t *testing.T) {
	c := ctx()
	got, err := Render("myapp_testing_{slug}", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "myapp_testing_proj_1234" {
		t.Errorf("got %q", got)
	}
	got, err = Render("phpunit-{slug_dash}-", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "phpunit-proj-1234-" {
		t.Errorf("got %q", got)
	}
}

func TestUnknownKeyErrors(t *testing.T) {
	_, err := Render("{nope}", ctx())
	if err == nil {
		t.Fatal("want error")
	}
	var re *RenderError
	if !errors.As(err, &re) || re.UnknownKey != "nope" {
		t.Errorf("wrong error: %v", err)
	}
}

func TestNTokenRequiredWhenUsed(t *testing.T) {
	if _, err := Render("test_{n}", ctx()); err == nil {
		t.Errorf("want error when {n} used without WithN")
	}
	c := ctx().WithN(3)
	got, err := Render("test_{n}", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "test_3" {
		t.Errorf("got %q", got)
	}
}

func TestPortTokensRenderFromMap(t *testing.T) {
	c := ctx().WithPorts(map[string]uint16{"octane": 8042, "webpack": 3042})
	got, err := Render("http://app.test:{port_octane}/assets:{port_webpack}", c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://app.test:8042/assets:3042" {
		t.Errorf("got %q", got)
	}
}

func TestPortTokenUnknownSlotErrors(t *testing.T) {
	c := ctx().WithPorts(map[string]uint16{"octane": 8042})
	_, err := Render("{port_typo}", c)
	if err == nil {
		t.Fatal("want error for unknown port slot")
	}
	var re *RenderError
	if !errors.As(err, &re) || re.UnknownKey != "port_typo" {
		t.Errorf("wrong error: %v", err)
	}
}

func TestPortTokenWithoutAnyPortsErrors(t *testing.T) {
	if _, err := Render("{port_octane}", ctx()); err == nil {
		t.Fatal("want error when ports map empty")
	}
}

func TestRenderMapResolvesAndFailsLoudly(t *testing.T) {
	vals := map[string]string{"failed": "2", "total": "7", "sep": " | "}
	got, err := RenderMap("{failed}{sep}{total}", vals)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2 | 7" {
		t.Errorf("got %q", got)
	}
	// A key absent from the map is an unknown-key error, mirroring
	// Render — a typo in a status format must not render as empty.
	_, err = RenderMap("{failed}/{totl}", vals)
	var re *RenderError
	if !errors.As(err, &re) || re.UnknownKey != "totl" {
		t.Errorf("wrong error: %v", err)
	}
}

func TestValidateAllowedPorts(t *testing.T) {
	if err := Validate("{port_octane}", Scope{AllowedPorts: []string{"octane"}}); err != nil {
		t.Errorf("want valid, got %v", err)
	}
	if err := Validate("{port_unlisted}", Scope{AllowedPorts: []string{"octane"}}); err == nil {
		t.Errorf("want error for unlisted port slot")
	}
	if err := Validate("{port_anything}", Scope{}); err == nil {
		t.Errorf("want error when scope disallows port tokens")
	}
}
