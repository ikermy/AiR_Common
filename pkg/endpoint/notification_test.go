package endpoint

import "testing"

func TestSimpleLocalizerParsesSupportedLanguages(t *testing.T) {
	for _, lang := range []string{"ru", "en", "es"} {
		loc, err := simpleLocalizer(lang)
		if err != nil {
			t.Fatalf("simpleLocalizer(%q) returned error: %v", lang, err)
		}
		got, err := loc.mustLocalize("operator.disconnected", nil)
		if err != nil {
			t.Fatalf("mustLocalize(%q) returned error: %v", lang, err)
		}
		if got == "" {
			t.Fatalf("mustLocalize(%q) returned empty string", lang)
		}
	}
}

func TestEventLocalizerLocalizerParsesSupportedLanguages(t *testing.T) {
	for _, lang := range []string{"ru", "en", "es"} {
		loc, err := eventLocalizer(lang)
		if err != nil {
			t.Fatalf("eventLocalizer(%q) returned error: %v", lang, err)
		}
		got, err := loc.mustLocalize("payment.status", nil)
		if err != nil {
			t.Fatalf("mustLocalize(%q) returned error: %v", lang, err)
		}
		if got == "" {
			t.Fatalf("mustLocalize(%q) returned empty string", lang)
		}
	}
}
