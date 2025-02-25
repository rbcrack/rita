package beaconproxy

import (
	"math"
	"sort"
	"sync"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/uconnproxy"
	"github.com/activecm/rita/util"

	"github.com/globalsign/mgo/bson"
	log "github.com/sirupsen/logrus"
)

type (
	//analyzer handles calculating statistical measures of the distribution of timestamps
	//between pairs of proxied hosts
	analyzer struct {
		tsMin            int64                      // min timestamp for the whole dataset
		tsMax            int64                      // max timestamp for the whole dataset
		chunk            int                        //current chunk (0 if not on rolling analysis)
		db               *database.DB               // provides access to MongoDB
		conf             *config.Config             // contains details needed to access MongoDB
		log              *log.Logger                // main logger for RITA
		analyzedCallback func(database.BulkChanges) // called on each analyzed result
		closedCallback   func()                     // called when .close() is called and no more calls to analyzedCallback will be made
		analysisChannel  chan *uconnproxy.Input     // holds unanalyzed data
		analysisWg       sync.WaitGroup             // wait for analysis to finish
	}
)

// newAnalyzer creates a new analyzer for calculating the beacon statistics of proxied unique connections
func newAnalyzer(min int64, max int64, chunk int, db *database.DB, conf *config.Config, log *log.Logger,
	analyzedCallback func(database.BulkChanges), closedCallback func()) *analyzer {
	return &analyzer{
		tsMin:            min,
		tsMax:            max,
		chunk:            chunk,
		db:               db,
		conf:             conf,
		log:              log,
		analyzedCallback: analyzedCallback,
		closedCallback:   closedCallback,
		analysisChannel:  make(chan *uconnproxy.Input),
	}
}

// collect gathers sorted proxied unique connection data for analysis
func (a *analyzer) collect(data *uconnproxy.Input) {
	a.analysisChannel <- data
}

// close waits for the analyzer to finish
func (a *analyzer) close() {
	close(a.analysisChannel)
	a.analysisWg.Wait()
	a.closedCallback()
}

// start kicks off a new analysis thread
func (a *analyzer) start() {
	a.analysisWg.Add(1)
	go func() {

		for entry := range a.analysisChannel {

			//store the diffFull slice length since we use it a lot
			//for timestamps this is one less then the data slice length
			//since we are calculating the times in between readings
			tsLength := len(entry.TsList) - 1

			//find the delta times between the timestamps and sort
			diffFull := make([]int64, tsLength)
			for i := 0; i < tsLength; i++ {
				interval := entry.TsList[i+1] - entry.TsList[i]
				diffFull[i] = interval
			}
			sort.Sort(util.SortableInt64(diffFull))

			// We are excluding delta zero for scoring calculations
			// but using a separate array that includes it for making
			// the user/ graph reference variables returned by createCountMap.

			// Search for the section of diffFull without any 0's in it
			// The dissector guarantees that there are at least three unique timestamps in res.TsList
			// as a result, we are guaranteed to find at least two non-zero intervals in diffFull
			diffNonZeroIdx := 0
			for i := 0; i < len(diffFull); i++ {
				if diffFull[i] > 0 {
					diffNonZeroIdx = i
					break
				}
			}

			diff := diffFull[diffNonZeroIdx:] // select the part of diffFull without any 0's

			//store the diff slice length
			diffLength := len(diff)

			//perfect beacons should have symmetric delta time and size distributions
			//Bowley's measure of skew is used to check symmetry
			tsSkew := float64(0)

			//diffLength-1 is used since diff is a zero based slice
			tsLow := diff[util.Round(.25*float64(diffLength-1))]
			tsMid := diff[util.Round(.5*float64(diffLength-1))]
			tsHigh := diff[util.Round(.75*float64(diffLength-1))]
			tsBowleyNum := tsLow + tsHigh - 2*tsMid
			tsBowleyDen := tsHigh - tsLow

			//tsSkew should equal zero if the denominator equals zero
			//bowley skew is unreliable if Q2 = Q1 or Q2 = Q3
			if tsBowleyDen != 0 && tsMid != tsLow && tsMid != tsHigh {
				tsSkew = float64(tsBowleyNum) / float64(tsBowleyDen)
			}

			//perfect beacons should have very low dispersion around the
			//median of their delta times
			//Median Absolute Deviation About the Median
			//is used to check dispersion
			devs := make([]int64, diffLength)
			for i := 0; i < diffLength; i++ {
				devs[i] = util.Abs(diff[i] - tsMid)
			}

			sort.Sort(util.SortableInt64(devs))

			tsMadm := devs[util.Round(.5*float64(diffLength-1))]

			//Store the range for human analysis
			tsIntervalRange := diff[diffLength-1] - diff[0]

			//get a list of the intervals found in the data,
			//the number of times the interval was found,
			//and the most occurring interval
			intervals, intervalCounts, tsMode, tsModeCount := createCountMap(diffFull)

			//more skewed distributions receive a lower score
			//less skewed distributions receive a higher score
			tsSkewScore := 1.0 - math.Abs(tsSkew) //smush tsSkew

			//lower dispersion is better
			tsMadmScore := 1.0
			if tsMid >= 1 {
				tsMadmScore = 1.0 - float64(tsMadm)/float64(tsMid)
			}
			if tsMadmScore < 0 {
				tsMadmScore = 0
			}

			// connection count scoring
			tsConnDiv := (float64(a.tsMax) - float64(a.tsMin)) / 3600
			tsConnCountScore := float64(entry.ConnectionCount) / tsConnDiv
			if tsConnCountScore > 1.0 {
				tsConnCountScore = 1.0
			}

			//score numerators
			tsSum := tsSkewScore + tsMadmScore + tsConnCountScore

			//score averages
			tsScore := math.Ceil((tsSum/3.0)*1000) / 1000
			score := math.Ceil((tsSum/3.0)*1000) / 1000

			// copy variables to be used by bulk callback to prevent capturing by reference
			pairSelector := entry.Hosts.BSONKey()
			proxyBeaconQuery := bson.M{
				"$set": bson.M{
					"connection_count":   entry.ConnectionCount,
					"proxy":              entry.Proxy,
					"src_network_name":   entry.Hosts.SrcNetworkName,
					"ts.range":           tsIntervalRange,
					"ts.mode":            tsMode,
					"ts.mode_count":      tsModeCount,
					"ts.intervals":       intervals,
					"ts.interval_counts": intervalCounts,
					"ts.dispersion":      tsMadm,
					"ts.skew":            tsSkew,
					"ts.conns_score":     tsConnCountScore,
					"ts.score":           tsScore,
					"score":              score,
					"cid":                a.chunk,
				},
			}

			update := database.BulkChanges{
				a.conf.T.BeaconProxy.BeaconProxyTable: []database.BulkChange{{
					Selector: pairSelector,
					Update:   proxyBeaconQuery,
					Upsert:   true,
				}},
			}

			a.analyzedCallback(update)
		}

		a.analysisWg.Done()
	}()
}

// createCountMap returns a distinct data array, data count array, the mode,
// and the number of times the mode occurred
func createCountMap(sortedIn []int64) ([]int64, []int64, int64, int64) {
	//Since the data is already sorted, we can call this without fear
	distinct, countsMap := countAndRemoveConsecutiveDuplicates(sortedIn)
	countsArr := make([]int64, len(distinct))
	mode := distinct[0]
	max := countsMap[mode]
	for i, datum := range distinct {
		count := countsMap[datum]
		countsArr[i] = count
		if count > max {
			max = count
			mode = datum
		}
	}
	return distinct, countsArr, mode, max
}

// countAndRemoveConsecutiveDuplicates removes consecutive
// duplicates in an array of integers and counts how many
// instances of each number exist in the array.
// Similar to `uniq -c`, but counts all duplicates, not just
// consecutive duplicates.
func countAndRemoveConsecutiveDuplicates(numberList []int64) ([]int64, map[int64]int64) {
	//Avoid some reallocations
	result := make([]int64, 0, len(numberList)/2)
	counts := make(map[int64]int64)

	last := numberList[0]
	result = append(result, last)
	counts[last]++

	for idx := 1; idx < len(numberList); idx++ {
		if last != numberList[idx] {
			result = append(result, numberList[idx])
		}
		last = numberList[idx]
		counts[last]++
	}
	return result, counts
}
