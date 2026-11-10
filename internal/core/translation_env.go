package core

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

func NewTranslator() Translator {
	timeout := 20 * time.Second
	if n, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("KIMI_WORK_TRANSLATION_TIMEOUT_MS")), 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n > 0 {
		timeout = time.Duration(min(max(math.Trunc(n), 1000), 120000)) * time.Millisecond
	}
	return Translator{Timeout: timeout}
}
