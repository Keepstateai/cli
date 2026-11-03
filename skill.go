// skill.go: ks skill (also ks --skill), the manual an agent reads before it
// delegates work to child agents. It is embedded in the binary, so it reads
// the same with no network and no sign-in, and it always matches the verbs
// of this build.
package main

import (
	_ "embed"
	"fmt"
)

//go:embed docs/skill.md
var skillManual string

func runSkill(*Invocation) { fmt.Print(skillManual) }
