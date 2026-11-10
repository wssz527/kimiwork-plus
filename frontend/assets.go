package frontend

import "embed"

//go:embed *.html *.css *.js
var Assets embed.FS
