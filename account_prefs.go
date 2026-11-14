// account_prefs.go: your own profile and your email choices, from the
// terminal, the same two pages the console's account menu shows.
//
//	ks profile show                                     name, title, email, picture
//	ks profile set [--name N] [--title T] [--picture on|off]
//	ks email show                                       your two email choices
//	ks email set [--onboarding on|off] [--changelog on|off]
//
// Both belong to your personal account. They take no organization context:
// an organization's context is never sent, so they read and change your own
// account whichever context is chosen. A change names only what was given,
// so what you did not name is left as it was.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	profileNameMax  = 80
	profileTitleMax = 80
)

type accountProfile struct {
	DisplayName string `json:"display_name"`
	Title       string `json:"title"`
	PictureURL  string `json:"picture_url"`
	ShowPicture bool   `json:"show_picture"`
	Initials    string `json:"initials"`
	Email       string `json:"email"`
}

type emailChoices struct {
	Onboarding    bool `json:"onboarding"`
	Changelog     bool `json:"changelog"`
	OnboardingMax int  `json:"onboarding_max"`
}

// accountPrefsErr renders a refusal in the service's own words, with the
// next step it names, and the plain line that nothing changed. The two
// choices the service refuses by name are usage errors, as a bad flag is.
func accountPrefsErr(err error, nothing string) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_patch_empty", "ks_profile_name_invalid", "ks_profile_title_invalid":
		ce.Code = exitUsage
	}
	if he.Status/100 == 4 && he.Status != http.StatusUnauthorized && he.Status != http.StatusForbidden && he.Status != http.StatusTooManyRequests && !strings.Contains(strings.ToLower(ce.Message), strings.ToLower(nothing)) {
		ce.Message += ". " + capitalFirst(nothing)
	}
	if ce.NextAction == "" {
		var doc struct {
			Error struct {
				NextAction string `json:"next_action"`
			} `json:"error"`
		}
		if json.Unmarshal(he.Raw, &doc) == nil && strings.TrimSpace(doc.Error.NextAction) != "" {
			ce.NextAction = sanitize(strings.TrimSpace(doc.Error.NextAction))
		}
	}
	return ce
}

func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// onOff reads an on or off flag: nothing else is a choice.
func onOff(inv *Invocation, flag, nothing string) (bool, bool) {
	if !inv.Set(flag) {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(inv.Str(flag))) {
	case "on":
		return true, true
	case "off":
		return false, true
	}
	fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--%s is on or off, not %q; %s", flag, inv.Str(flag), nothing)})
	return false, false
}

func pictureWords(p accountProfile) string {
	switch {
	case !p.ShowPicture:
		return "off (your initials show instead)"
	case p.PictureURL == "":
		return "on, and you have no picture yet (your initials show)"
	}
	return "on (your sign-in picture shows)"
}

func printProfile(p accountProfile) {
	title := p.Title
	if title == "" {
		title = "(none)"
	}
	fmt.Printf("Name:     %s\n", p.DisplayName)
	fmt.Printf("Title:    %s\n", title)
	fmt.Printf("Email:    %s\n", p.Email)
	fmt.Printf("Initials: %s\n", p.Initials)
	fmt.Printf("Picture:  %s\n", pictureWords(p))
}

func hostedProfileShow(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data accountProfile `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/profile", nil, &env); err != nil {
		die(accountPrefsErr(err, "nothing was read"))
	}
	p := env.Data
	emit(p, func() { printProfile(p) })
}

func hostedProfileSet(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	body := map[string]any{}
	if inv.Set("name") {
		name := inv.Str("name")
		switch n := utf8.RuneCountInString(strings.TrimSpace(name)); {
		case n == 0:
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--name cannot be empty; " + nothing})
		case n > profileNameMax:
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--name is at most %d characters (this one is %d); %s", profileNameMax, n, nothing)})
		}
		body["display_name"] = name
	}
	if inv.Set("title") {
		title := inv.Str("title")
		if n := utf8.RuneCountInString(strings.TrimSpace(title)); n > profileTitleMax {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--title is at most %d characters (this one is %d); %s", profileTitleMax, n, nothing)})
		}
		body["title"] = title // an empty title clears it
	}
	if show, ok := onOff(inv, "picture", nothing); ok {
		body["show_picture"] = show
	}
	if len(body) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give --name, --title or --picture; " + nothing, NextAction: "ks profile show"})
	}
	var env struct {
		Data accountProfile `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/profile", body, &env); err != nil {
		die(accountPrefsErr(err, nothing))
	}
	p := env.Data
	emit(p, func() {
		fmt.Println("Your profile is saved.")
		printProfile(p)
	})
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func printEmailChoices(c emailChoices) {
	n := c.OnboardingMax
	if n < 1 {
		n = 3
	}
	fmt.Printf("Onboarding emails: %s  (up to %d in the first 14 days after you sign up)\n", onOffWord(c.Onboarding), n)
	fmt.Printf("Changelog email:   %s  (a note about what is new)\n", onOffWord(c.Changelog))
	fmt.Println("Mail about your account always arrives and no choice here stops it: sign-in codes, alerts, statements and security notices.")
}

func hostedEmailShow(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data emailChoices `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/email-preferences", nil, &env); err != nil {
		die(accountPrefsErr(err, "nothing was read"))
	}
	c := env.Data
	emit(c, func() {
		printEmailChoices(c)
		fmt.Println("Change them with: ks email set --onboarding on|off --changelog on|off")
	})
}

func hostedEmailSet(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	body := map[string]any{}
	if v, ok := onOff(inv, "onboarding", nothing); ok {
		body["onboarding"] = v
	}
	if v, ok := onOff(inv, "changelog", nothing); ok {
		body["changelog"] = v
	}
	if len(body) == 0 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "give --onboarding or --changelog, each on or off; " + nothing, NextAction: "ks email show"})
	}
	var env struct {
		Data emailChoices `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", "/api/v2/email-preferences", body, &env); err != nil {
		die(accountPrefsErr(err, nothing))
	}
	c := env.Data
	emit(c, func() {
		fmt.Println("Your email choices are saved.")
		printEmailChoices(c)
	})
}
