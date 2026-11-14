package kula

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"testing"
)

// The landing page pins landing.js with a Subresource Integrity hash, and a
// browser refuses a script that no longer matches it: landing/gen_sri.sh must
// be run after every change to landing.js.
func TestLandingPagePinsItsScript(t *testing.T) {
	page, err := os.ReadFile("landing/index.html")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("this tree has no landing page")
	}
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("landing/landing.js")
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`<script src="landing\.js" integrity="(sha384-[^"]+)"`).FindSubmatch(page)
	if pinned == nil {
		t.Fatal("landing/index.html does not load landing.js with an integrity hash")
	}
	sum := sha512.Sum384(script)
	if want := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); string(pinned[1]) != want {
		t.Errorf("landing/index.html pins landing.js as %s, the file is %s; run landing/gen_sri.sh", pinned[1], want)
	}
}
