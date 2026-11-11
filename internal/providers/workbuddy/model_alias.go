package workbuddy

import (
	"os"
	"strings"
)

// intlModelAliases maps a client-visible logical id to the id the intl
// upstream currently accepts. The intl exit renamed deepseek-v4.1-flash to the
// "-sg" build; keeping the logical id in catalog/client responses avoids
// changing API-key allowlists and usage attribution.
var intlModelAliases = map[string]string{
	"deepseek-v4.1-flash": "deepseek-v4.1-flash-sg",
}

func intlModelAliasEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_INTL_MODEL_ALIAS"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func intlModelAlias(name, realm string) string {
	if realm != realmIntl || !intlModelAliasEnabled() {
		return name
	}
	if target, ok := intlModelAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return target
	}
	return name
}
