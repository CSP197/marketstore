package main

import (
	"flag"
	"fmt"
	"github.com/gobwas/glob"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/alpacahq/marketstore/v4/contrib/calendar"
	"github.com/alpacahq/marketstore/v4/contrib/ondiskagg/aggtrigger"
	"github.com/alpacahq/marketstore/v4/contrib/polygon/api"
	"github.com/alpacahq/marketstore/v4/contrib/polygon/backfill"
	"github.com/alpacahq/marketstore/v4/executor"
	"github.com/alpacahq/marketstore/v4/plugins/trigger"
	"github.com/alpacahq/marketstore/v4/utils"
	"github.com/alpacahq/marketstore/v4/utils/log"
)

var (
	dir, from, to, barPeriod string
	bars, quotes, trades     bool
	symbols                  string
	parallelism              int
	apiKey                   string
	exchanges                string
	batchSize                int

	// NY timezone
	NY, _  = time.LoadLocation("America/New_York")
	format = "2006-01-02"
)

func init() {
	flag.StringVar(&dir, "dir", "/project/data", "mktsdb directory to backfill to")
	flag.StringVar(&from, "from", time.Now().Add(-365*24*time.Hour).Format(format), "backfill from date (YYYY-MM-DD) [included]")
	flag.StringVar(&to, "to", time.Now().Format(format), "backfill to date (YYYY-MM-DD) [not included]")
	flag.StringVar(&exchanges, "exchanges", "*", "comma separated list of exchange")
	flag.BoolVar(&bars, "bars", false, "backfill bars")
	flag.StringVar(&barPeriod, "bar-period", (time.Hour * 24).String(), "backfill bar period")
	flag.BoolVar(&quotes, "quotes", false, "backfill quotes")
	flag.BoolVar(&trades, "trades", false, "backfill trades")
	flag.StringVar(&symbols, "symbols", "*",
		"glob pattern of symbols to backfill, the default * means backfill all symbols")
	flag.IntVar(&parallelism, "parallelism", runtime.NumCPU(), "parallelism (default NumCPU)")
	flag.IntVar(&batchSize, "batchSize", 50000, "batch/pagination size for downloading trades & quotes")
	flag.StringVar(&apiKey, "apiKey", "", "polygon API key")

	flag.Parse()
}

func main() {
	// free memory in the background every 1 minute for long running
	// backfills with very high parallelism
	go func() {
		for {
			<-time.After(time.Minute)
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			memStart := m.Alloc
			log.Info("freeing memory...")
			debug.FreeOSMemory()
			runtime.ReadMemStats(&m)
			memEnd := m.Alloc
			log.Info(
				"mem stats: [start: %v end: %v freed: %v]",
				bytefmt.ByteSize(memStart),
				bytefmt.ByteSize(memEnd),
				bytefmt.ByteSize(memStart-memEnd),
			)
		}
	}()

	initWriter()

	if apiKey == "" {
		log.Fatal("[polygon] api key is required")
	}

	api.SetAPIKey(apiKey)

	start, err := time.Parse(format, from)
	if err != nil {
		log.Fatal("[polygon] failed to parse from timestamp (%v)", err)
	}

	end, err := time.Parse(format, to)
	if err != nil {
		log.Fatal("[polygon] failed to parse to timestamp (%v)", err)
	}

	barPeriodDuration, err := time.ParseDuration(barPeriod)
	if err != nil {
		log.Fatal("[polygon] failed to parse bar-period duration (%v)", err)
	}
	if barPeriodDuration < 24*time.Hour {
		barPeriodDuration = 24 * time.Hour
	}
	if barPeriodDuration > 60*24*time.Hour {
		barPeriodDuration = 60 * 24 * time.Hour
	}

	var symbolList []string
	log.Info("[polygon] listing symbols for pattern: %v", symbols)
	pattern := glob.MustCompile(symbols)
	resp, err := api.ListTickers()
	if err != nil {
		log.Fatal("[polygon] failed to list symbols (%v)", err)
	}
	log.Info("[polygon] %v symbols available", len(resp.Tickers))
	symbolList = make([]string, 1)
	for _, s := range resp.Tickers {
		if pattern.Match(s.Ticker) {
			symbolList = append(symbolList, s.Ticker)
		}
	}
	symbolList = unique(symbolList)
	log.Info("[polygon] selected %v symbols", len(symbolList))

	var exchangeIDs []int
	if exchanges != "*" {
		for _, exchangeIDStr := range strings.Split(exchanges, ",") {
			exchangeIDInt, err := strconv.Atoi(exchangeIDStr)
			if err != nil {
				log.Fatal("Invalid exchange ID: %v", exchangeIDStr)
			}

			exchangeIDs = append(exchangeIDs, exchangeIDInt)
		}
	}

	sem := make(chan struct{}, parallelism)

	tt := time.Now()
	if bars {
		log.Info("[polygon] backfilling bars from %v to %v", start, end)

		for _, sym := range symbolList {
			if sym == "" {
				continue
			}
			sem <- struct{}{}
			go func(currentSymbol string) {
				defer func() { <-sem }()

				s := start
				e := end
				addPeriod := barPeriodDuration
				if len(exchangeIDs) != 0 && addPeriod != 24*time.Hour {
					log.Warn("[polygon] bar period not adjustable when exchange filtered")
					addPeriod = 24 * time.Hour
				}
				log.Info("[polygon] backfilling bars for %v", currentSymbol)
				for e.After(s) {
					if calendar.Nasdaq.IsMarketDay(s) {
						log.Info("[polygon] backfilling bars for %v on %v", currentSymbol, s)

						if s.Add(addPeriod).After(e) {
							addPeriod = e.Sub(s)
						}

						if len(exchangeIDs) == 0 {
							if err = backfill.Bars(currentSymbol, s, s.Add(addPeriod)); err != nil {
								log.Warn("[polygon] failed to backfill bars for %v (%v)", currentSymbol, err)
							}
						} else {
							if err = backfill.BuildBarsFromTrades(currentSymbol, s, exchangeIDs, batchSize); err != nil {
								log.Warn("[polygon] failed to backfill bars for %v @ %v (%v)", currentSymbol, s, err)
							}
						}

					}
					s = s.Add(addPeriod)
				}
			}(sym)
		}
	}

	if quotes {
		log.Info("[polygon] backfilling quotes from %v to %v", start, end)

		for _, sym := range symbolList {
			s := start
			e := end

			log.Info("[polygon] backfilling quotes for %v", sym)

			for e.After(s) {
				if calendar.Nasdaq.IsMarketDay(s) {
					log.Info("[polygon] backfilling quotes for %v on %v", sym, s)

					sem <- struct{}{}
					go func(t time.Time) {
						defer func() { <-sem }()

						if err = backfill.Quotes(sym, t, t.Add(24*time.Hour), batchSize); err != nil {
							log.Warn("[polygon] failed to backfill quotes for %v (%v)", sym, err)
						}
					}(s)
				}
				s = s.Add(24 * time.Hour)
			}
		}
	}

	if trades {
		log.Info("[polygon] backfilling trades from %v to %v", start, end)

		for _, sym := range symbolList {
			s := start
			e := end

			log.Info("[polygon] backfilling trades for %v", sym)

			for e.After(s) {
				log.Info("Checking %v", s)
				if calendar.Nasdaq.IsMarketDay(s) {
					log.Info("[polygon] backfilling trades for %v on %v", sym, s)

					sem <- struct{}{}
					go func(t time.Time) {
						defer func() { <-sem }()

						if err = backfill.Trades(sym, t, batchSize); err != nil {
							log.Warn("[polygon] failed to backfill trades for %v @ %v (%v)", sym, t, err)
						}
					}(e)
				}
				s = s.Add(24 * time.Hour)
			}
		}
	}

	// make sure all goroutines finish
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	log.Info("[polygon] api call duration %s", backfill.ApiCallDuration)
	log.Info("[polygon] backfilling complete %s", time.Now().Sub(tt).String())
	log.Info("[polygon] waiting for 10 more seconds for ondiskagg triggers to complete")
	time.Sleep(10 * time.Second)
}

func initWriter() {
	utils.InstanceConfig.Timezone = NY
	utils.InstanceConfig.WALRotateInterval = 5

	executor.NewInstanceSetup(
		fmt.Sprintf("%v/mktsdb", dir),
		true, true, true, true)

	config := map[string]interface{}{
		"filter":       "nasdaq",
		"destinations": []string{"5Min", "15Min", "1H", "1D"},
	}

	trig, err := aggtrigger.NewTrigger(config)
	if err != nil {
		log.Fatal("[polygon] backfill failed to initialize writer (%v)", err)
	}

	executor.ThisInstance.TriggerMatchers = []*trigger.TriggerMatcher{
		trigger.NewMatcher(trig, "*/1Min/OHLCV"),
	}
}

func unique(stringSlice []string) []string {
	var list []string
	keys := make(map[string]bool)
	for _, entry := range stringSlice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}
