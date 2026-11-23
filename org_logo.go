// org_logo.go: an organization's logo from the terminal. An owner or admin
// sets one picture (a PNG, JPEG or WebP of 1 MB at most, 64 to 4096 pixels a
// side) or removes it; every member sees it in the console's header. The
// service decides what the file is from its first bytes, fits it inside a
// 256 pixel square and keeps only the PNG it made, so what is sent is never
// served back as it was. Its words are printed as they come.
package main

import (
	"net/url"
	"os"
	"strings"
)

const orgLogoMaxBytes = 1 << 20

type customerOrgLogo struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Tile   string `json:"tile"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func orgLogoPath(org string) string { return "/api/v2/orgs/" + url.PathEscape(org) + "/logo" }

func hostedOrgLogoSet(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Str("organization"), nothing)
	file := strings.TrimSpace(inv.Arg(0))
	if file == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name the picture file; " + nothing})
	}
	tile := strings.TrimSpace(inv.Str("tile"))
	if inv.Set("tile") && tile != "light" && tile != "dark" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "a tile is light or dark; " + nothing})
	}
	st, err := os.Stat(file)
	if err != nil || st.IsDir() {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "cannot read " + file + " as a picture file; " + nothing})
	}
	if st.Size() > orgLogoMaxBytes {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "that image is over 1 MB; " + nothing})
	}
	b, err := os.ReadFile(file)
	if err != nil {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "cannot read " + file + "; " + nothing})
	}
	path := orgLogoPath(org)
	if tile != "" {
		path += "?" + url.Values{"tile": {tile}}.Encode()
	}
	var env struct {
		Data customerOrgLogo `json:"data"`
	}
	if err := hostedCall(cr, "PUT", path, b, &env); err != nil {
		die(orgErr(err, nothing))
	}
	d := env.Data
	emit(map[string]any{"organization": org, "logo": d}, func() {
		outf("%s logo set: %d x %d px, on a %s tile (kept as a 256 px PNG, sha256 %s)\n", org, d.Width, d.Height, d.Tile, d.SHA256)
		outln("Members see it in the console's header; the original file is not kept.")
	})
}

func hostedOrgLogoRemove(cr hostedCreds, inv *Invocation) {
	const nothing = "nothing was changed"
	org := orgArgOrContext(cr, inv.Str("organization"), nothing)
	var env struct {
		Data struct {
			Removed bool `json:"removed"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "DELETE", orgLogoPath(org), nil, &env); err != nil {
		die(orgErr(err, nothing))
	}
	removed := env.Data.Removed
	emit(map[string]any{"organization": org, "removed": removed}, func() {
		if removed {
			outf("%s logo removed; its initials show instead.\n", org)
			return
		}
		outf("%s had no logo; nothing changed.\n", org)
	})
}
