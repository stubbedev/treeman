package template

import (
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
	if re, ok := err.(*RenderError); !ok || re.UnknownKey != "nope" {
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
