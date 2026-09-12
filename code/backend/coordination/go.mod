// TEMPORARY standalone module.
//
// These are the pure, dependency-free coordination functions. They are being
// written in parallel with the nuzur schema, before the generated backend tree
// exists. Once codegen has run, this directory moves to
// code/backend/metiche/app/coordination/ and this go.mod is deleted so the
// package folds into the backend module. Nothing here may import generated code.
module github.com/mklfarha/metiche/coordination

go 1.26

require github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
