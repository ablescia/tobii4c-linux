// Package udev incorpora la regola udev nel binario (`tobii4c udev`).
package udev

import _ "embed"

// Rules è il contenuto di 70-tobii-4c.rules.
//
//go:embed 70-tobii-4c.rules
var Rules string

// FileName è il nome con cui installarla in /etc/udev/rules.d/.
const FileName = "70-tobii-4c.rules"
