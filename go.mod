module agentfs

go 1.26.0

require (
	github.com/clems4ever/all-minilm-l6-v2-go v0.0.9
	github.com/fsnotify/fsnotify v1.9.0
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82
	github.com/sugarme/tokenizer v0.3.0
	github.com/yalue/onnxruntime_go v1.21.0
	modernc.org/sqlite v1.38.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/patrickmn/go-cache v2.1.0+incompatible // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/schollz/progressbar/v2 v2.15.0 // indirect
	github.com/sugarme/regexpset v0.0.0-20200920021344-4d4ec8eaf93c // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	modernc.org/libc v1.66.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Local copy carries the real 90MB all-MiniLM-L6-v2 model (the upstream Go
// module only ships a Git LFS pointer, so go:embed would embed a 133-byte stub).
replace github.com/clems4ever/all-minilm-l6-v2-go => ./third_party/all-minilm-l6-v2-go

// The ONNX embedder needs a tokenizer fork with NewRawInputSequence. The library
// declares this replace itself, but replace directives in dependencies are
// ignored by the main module, so it must be repeated here.
replace github.com/sugarme/tokenizer => github.com/clems4ever/tokenizer v0.0.0-20250926133620-9ddc80533c43
