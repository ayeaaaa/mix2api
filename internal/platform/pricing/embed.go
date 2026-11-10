package pricing

import _ "embed"

// embeddedJSON is the catalogue shipped inside the binary. It is a verbatim
// copy of v1.6.5 app/pricing/pricing.json, including the OpenRouter source
// attribution in meta.note and meta.sources.
//
//go:embed data/pricing.json
var embeddedJSON []byte

func readEmbedded() (document, error) { return decodeDocument(embeddedJSON) }
