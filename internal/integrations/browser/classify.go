package browser

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/perishabledave/bosun/internal/integration"
)

// elemInfo describes a page element as the page script reports it. All
// strings come from the page and are untrusted.
type elemInfo struct {
	Tag             string `json:"tag"`  // upper case, e.g. "BUTTON"
	Type            string `json:"type"` // lower case type attribute ("" if none)
	Role            string `json:"role"`
	Name            string `json:"name"`  // accessible name
	Text            string `json:"text"`  // visible text
	Value           string `json:"value"` // value of button-like inputs only
	AriaLabel       string `json:"aria_label"`
	Title           string `json:"title"`
	InForm          bool   `json:"in_form"`
	FormMethod      string `json:"form_method"` // lower case; "get" by default
	FormAction      string `json:"form_action"`
	FormHasPassword bool   `json:"form_has_password"`
	Href            string `json:"href"`
	Editable        bool   `json:"editable"` // text-entry element
	Password        bool   `json:"password"` // input type=password
	Disabled        bool   `json:"disabled"`
	Select          bool   `json:"select"` // native <select>
}

// stateWords are the labels that mark a button or link as one that changes
// account state (the submit gate). Matched case-insensitively on word
// boundaries.
var stateWords = regexp.MustCompile(`(?i)\b(buy|pay|purchase|order|checkout|check out|confirm|book|reserve|send|post|publish|delete|remove|cancel|unsubscribe|save|update|change|apply|submit|sign up|signup|register|transfer)\b`)

// stateChanging reports whether clicking e may change account state, with a
// reason that contains no page text (it is shown to the model). browser.click
// refuses such elements; browser.submit (always approved) handles them.
func stateChanging(e elemInfo) (bool, string) {
	tag, typ := strings.ToUpper(e.Tag), strings.ToLower(e.Type)
	switch {
	case tag == "INPUT" && (typ == "submit" || typ == "image"):
		return true, "it is a form submit input"
	case tag == "BUTTON" && typ == "submit":
		return true, "it is a submit button"
	case tag == "BUTTON" && typ == "" && e.InForm:
		return true, "it is a button inside a form (buttons submit forms by default)"
	}
	for _, s := range []string{e.Name, e.Text, e.Value, e.AriaLabel, e.Title} {
		if m := stateWords.FindString(s); m != "" {
			return true, fmt.Sprintf("its label contains %q", strings.ToLower(m))
		}
	}
	return false, ""
}

// enterSubmits reports whether pressing Enter in a field of a form described
// by e may change account state: anything but a GET form without a password
// field (a search box) is treated as a submission.
func enterSubmits(e elemInfo) bool {
	if !e.InForm {
		return false
	}
	return (e.FormMethod != "" && e.FormMethod != "get") || e.FormHasPassword
}

// sitePatterns is a compiled sensitive_sites list. A pattern "chase.com" or
// "*.chase.com" matches chase.com and every subdomain of it.
type sitePatterns []string

func compileSites(list []string) (sitePatterns, error) {
	var out sitePatterns
	for _, p := range list {
		raw := strings.TrimSpace(p)
		s := integration.NormalizeScope(strings.TrimPrefix(raw, "*."))
		if s == "" {
			return nil, fmt.Errorf("sensitive_sites: %q is not a site pattern", p)
		}
		out = append(out, s)
	}
	return out, nil
}

// match reports whether site (normalized) is sensitive.
func (p sitePatterns) match(site string) bool {
	if site == "" {
		return false
	}
	for _, s := range p {
		if site == s || strings.HasSuffix(site, "."+s) {
			return true
		}
	}
	return false
}

// defaultSensitiveSites are banks, brokerages, payment and identity
// providers: every browser action on them asks, whatever the rules say.
var defaultSensitiveSites = []string{
	"*.chase.com", "*.bankofamerica.com", "*.wellsfargo.com", "*.citi.com", "*.capitalone.com",
	"*.usbank.com", "*.americanexpress.com", "*.discover.com", "*.schwab.com", "*.fidelity.com",
	"*.vanguard.com", "*.etrade.com", "*.robinhood.com", "*.coinbase.com", "*.paypal.com",
	"*.venmo.com", "*.wise.com", "*.stripe.com", "accounts.google.com", "myaccount.google.com",
	"pay.google.com", "appleid.apple.com", "*.icloud.com", "login.microsoftonline.com",
	"account.microsoft.com", "1password.com", "*.1password.com", "*.bitwarden.com", "*.lastpass.com",
	"*.irs.gov", "*.ssa.gov", "*.login.gov",
}
