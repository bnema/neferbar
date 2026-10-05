// Package shaders holds the committed SPIR-V compiled from the sibling GLSL
// sources. Consumers do not need glslc; maintainers run go generate.
package shaders

import _ "embed"

//go:generate glslc -O -fshader-stage=vertex -o cell.vert.spv cell.vert
//go:generate glslc -O -fshader-stage=fragment -o cell.frag.spv cell.frag

//go:embed cell.vert.spv
var CellVertex []byte

//go:embed cell.frag.spv
var CellFragment []byte
