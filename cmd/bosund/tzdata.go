package main

// Embed the IANA time zone database so daemon.timezone works on hosts
// without zoneinfo files.
import _ "time/tzdata"
