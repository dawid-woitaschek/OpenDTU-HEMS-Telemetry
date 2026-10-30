package web

import "embed"

//go:embed *.html assets/*
var Files embed.FS
