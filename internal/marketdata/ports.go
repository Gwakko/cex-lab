package marketdata

import "github.com/gwakko/cex-lab/internal/marketdata/internal/hub"

// Metrics receives hub measurements (adapter: internal/app/instrument).
// Events come in through Hub.PublishTrade and Hub.PublishBookDelta, which
// match trading's MarketDataSink port; internal/app connects the two.
type Metrics = hub.Metrics
