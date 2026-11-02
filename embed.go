// Package ucompute holds files embedded into the ucompute command.
package ucompute

import _ "embed"

// Skill is the Claude Code skill that `ucompute setup` installs.
//
//go:embed skills/ucompute/SKILL.md
var Skill string
