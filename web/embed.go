// Package web 嵌入 templates + static 目录到二进制中。
package web

import (
	"embed"
)

//go:embed templates
var Templates embed.FS

//go:embed static
var Static embed.FS
