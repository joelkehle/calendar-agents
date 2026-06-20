package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/travelknowledge"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

// Reconciliation watcher (SCHEDULER_TRAVEL_SPEC §7): a background loop inside
// the scheduler agent that makes Joel's OWN offsite meetings get travel
// protection, and keeps travel blocks attached when parents move.
//
// Deployment dependency (normative, §7 preamble): correctness assumes the
// read agent runs with OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS=true. When
// false, private events are masked to "Private appointment" with a blanked
// location: private offsite meetings are then only detectable via the yellow
// category and private travel blocks cannot match the summary grammar.
// Defense-in-depth: events whose summary is exactly "Private appointment" are
// busy-but-untouchable — they count for adjacency and shrinking but the
// watcher never creates travel for them and never mutates them.
//
// Honest detection limits (§7.4):
//   - events-list does not return event descriptions, so travel_for= audit
//     lines are unreadable; the parent linkage is NOT recoverable from the
//     body. Summary grammar + same-date time adjacency is the ONLY linkage.
//   - the grammar carries the parent's boundary HH:MM: before blocks use the
//     parent start, return blocks use the parent end. Legacy return blocks
//     using the parent start are still recognized. Two meetings with the same
//     boundary minute on the same date are ambiguous; the watcher
//     must then do nothing (no move, no cancel) and emit
//     watch_travel_ambiguous.
//   - a parent moved across midnight, or whose travel block was manually
//     retitled, is unlinkable; its old block will look orphaned and its new
//     slot will get fresh blocks. Net effect is correct but transits through
//     a cancel+insert rather than a move.
//   - human-created events that happen to match the grammar are
//     indistinguishable from watcher blocks; mutation attempts on them fail
//     writer-side (not_owned/no marker) and are logged + skipped — the
//     writer's ownership markers, not the watcher's parsing, are the safety
//     boundary.
//
// All watcher state is in-memory; restart safety = writer idempotency cache +
// deterministic keys. It never deletes anything (no delete capability
// exists), never touches guard blocks, never mutates an event without an
// entry_id, never cancels during a truncated tick, never recreates a block
// Joel deleted (within the §7.3 detection limits), and never exceeds its
// per-tick mutation cap.

const (
	maxWatchMutationsPerTick = 6
	watchBackoffFailures     = 3
	watchMaxResults          = 200
	maskedPrivateSummary     = "Private appointment"
	meetingBufferSummary     = "Meeting Buffer"
)

// travelSummaryRE is the §7.4 summary grammar. Group 1 = dest (opaque, never
// re-validated), group 2 = side, group 3 = parent local boundary HH:MM.
var travelSummaryRE = regexp.MustCompile(`^Travel: (.+) \((for|return) (([01]\d|2[0-3]):([0-5]\d))\)$`)

type travelWatcher struct {
	agent *Agent

	// succeededInserts holds insert meta keys whose LIVE insert reported
	// success (fresh or replayed) this process lifetime; a key needed again
	// means Joel deleted the block — do not fight him.
	succeededInserts map[string]bool
	// humanRemoved denylists (parent id, parent start, side) for the process
	// lifetime after a detected human deletion.
	humanRemoved map[string]bool
	// failures counts consecutive writer failures per meta key; at
	// watchBackoffFailures the key joins backoff for the process lifetime.
	failures map[string]int
	backoff  map[string]bool
	// notOwned permanently skips event ids the writer refused as not ours.
	notOwned map[string]bool

	// lastCommunicationProposals is reset each plan tick and is intentionally
	// non-authoritative: it makes read/propose travel-conflict output
	// inspectable without creating email drafts or durable state.
	lastCommunicationProposals []watchCommunicationProposal
	proposalKeys               map[string]bool

	loggedDegraded bool
}

func newTravelWatcher(a *Agent) *travelWatcher {
	return &travelWatcher{
		agent:            a,
		succeededInserts: make(map[string]bool),
		humanRemoved:     make(map[string]bool),
		failures:         make(map[string]int),
		backoff:          make(map[string]bool),
		notOwned:         make(map[string]bool),
		proposalKeys:     make(map[string]bool),
	}
}

// run is the watcher loop. WatchIntervalMin <= 0 disables the watcher
// entirely. First tick after a random 0-60 s jitter post-startup.
func (w *travelWatcher) run(ctx context.Context) {
	if w.agent.cfg.WatchIntervalMin <= 0 {
		return
	}
	jitter := time.Duration(rand.Intn(61)) * time.Second
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	interval := time.Duration(w.agent.cfg.WatchIntervalMin) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// watchEvent is one scanned calendar event with parsed bounds.
type watchEvent struct {
	ev     calendarread.Event
	start  time.Time
	end    time.Time
	allDay bool
}

func (e watchEvent) entryID() string {
	if e.ev.ExtendedProperties == nil || e.ev.ExtendedProperties.Private == nil {
		return ""
	}
	return strings.TrimSpace(e.ev.ExtendedProperties.Private["entry_id"])
}

// watchMutation is one planned writer mutation for a tick.
type watchMutation struct {
	kind      string // "insert" | "move" | "cancel"
	key       string // writer meta.request_id (schedw-*)
	payload   outlookcalendarwrite.Request
	eventID   string // synthetic id of the mutated travel block ("" for inserts)
	parentKey string // human-removed bookkeeping for inserts
}

func (w *travelWatcher) tick(ctx context.Context) {
	a := w.agent
	a.metrics.IncCounter("watch_ticks")
	if a.knowledge == nil {
		if !w.loggedDegraded {
			w.loggedDegraded = true
		}
		log.Printf("%s watcher idle: travel knowledge not loaded", a.cfg.AgentID)
		return
	}
	loc := loadLocation()
	now := a.now().In(loc)
	session := a.newUpstreamSession("watch|" + now.Format(time.RFC3339))
	defer a.closeUpstreamSession(session)

	events, degraded, err := w.scan(ctx, session, now)
	if err != nil {
		log.Printf("%s watcher scan failed: %v", a.cfg.AgentID, err)
		return
	}
	if degraded {
		log.Printf("%s watcher scan degraded for at least one date: cancels suppressed this tick", a.cfg.AgentID)
	}
	mutations := w.plan(events, now, degraded)
	w.execute(ctx, session, mutations)
}

// scan fetches today + WatchHorizonDays local dates, one events-list each
// with MaxResults 200. The read agent clamps to 200 and returns no
// truncation flag, so truncation is detected as len(events) == MaxResults.
func (w *travelWatcher) scan(ctx context.Context, session upstreamSession, now time.Time) ([]watchEvent, bool, error) {
	a := w.agent
	loc := loadLocation()
	today := localDateStart(now, loc)
	var out []watchEvent
	degraded := false
	for day := 0; day <= a.cfg.WatchHorizonDays; day++ {
		date := today.AddDate(0, 0, day)
		req := calendarread.Request{
			Action: "events-list",
			Query: calendarread.EventsQuery{
				TimeMin:      date.Format(time.RFC3339),
				TimeMax:      date.AddDate(0, 0, 1).Format(time.RFC3339),
				SingleEvents: true,
				OrderBy:      "startTime",
				MaxResults:   watchMaxResults,
			},
		}
		raw, err := a.requestUpstream(ctx, session, a.cfg.CalendarReadAgent, "watch-read-"+date.Format("20060102"), req, "")
		if err != nil {
			w.recordScanDateFailure(date, err)
			degraded = true
			continue
		}
		var resp calendarread.EventsListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			w.recordScanDateFailure(date, fmt.Errorf("decode watcher calendar response: %w", err))
			degraded = true
			continue
		}
		if strings.TrimSpace(resp.Error) != "" {
			w.recordScanDateFailure(date, errors.New(resp.Error))
			degraded = true
			continue
		}
		if len(resp.Events) >= watchMaxResults {
			a.metrics.IncCounter("watch_events_truncated")
			log.Printf("%s watcher scan truncated date=%s events=%d max=%d", a.cfg.AgentID, date.Format("2006-01-02"), len(resp.Events), watchMaxResults)
			degraded = true
		}
		for _, event := range resp.Events {
			start, end, allDay, ok := eventBounds(event, loc)
			if !ok {
				continue
			}
			out = append(out, watchEvent{ev: event, start: start, end: end, allDay: allDay})
		}
	}
	return out, degraded, nil
}

func (w *travelWatcher) recordScanDateFailure(date time.Time, err error) {
	w.agent.metrics.IncCounter("watch_scan_date_failed")
	log.Printf("%s watcher scan date failed date=%s err=%v", w.agent.cfg.AgentID, date.Format("2006-01-02"), err)
}

// plan classifies the scan and produces this tick's mutations: missing-block
// inserts for offsite meetings (§7.3) and reattach/orphan handling for
// grammar-matching travel blocks (§7.5).
func (w *travelWatcher) plan(events []watchEvent, now time.Time, degraded bool) []watchMutation {
	a := w.agent
	loc := loadLocation()
	w.lastCommunicationProposals = nil
	w.proposalKeys = make(map[string]bool)

	var allDay []watchEvent       // origin/return context only; never busy/shrink/mutation input
	var neighbors []watchEvent    // shrink + adjacency set: timed, busy, non-guard, non-cancelled (incl. Travel: and masked private)
	var travelBlocks []watchEvent // "Travel: "-prefixed, busy, non-cancelled (ownership-blind adjacency)
	var grammarBlocks []watchEvent
	var anchors []watchEvent // §7.5(2): busy meetings excluding travel blocks
	var offsite []watchEvent

	for _, event := range events {
		summary := strings.TrimSpace(event.ev.Summary)
		if event.allDay {
			allDay = append(allDay, event)
			continue // skip 1 for travel creation, but keep as origin context
		}
		if strings.EqualFold(strings.TrimSpace(event.ev.Transparency), "transparent") {
			continue // skip 2
		}
		if isGuardSummary(summary) {
			continue // skip 3
		}
		if strings.HasPrefix(summary, outlookcalendarwrite.CancelledPrefix) {
			continue // skip 4
		}
		if summary == meetingBufferSummary {
			continue
		}
		neighbors = append(neighbors, event)
		if strings.HasPrefix(summary, outlookcalendarwrite.TravelSummaryPrefix) {
			travelBlocks = append(travelBlocks, event) // skip 5: never travel-for-travel
			if travelSummaryRE.MatchString(summary) {
				grammarBlocks = append(grammarBlocks, event)
			}
			continue
		}
		anchors = append(anchors, event)
		if summary == maskedPrivateSummary {
			// Masked-private sentinel (§7 preamble): counts for adjacency and
			// shrinking, never gets travel, never mutated.
			a.metrics.IncCounter("watch_travel_skipped")
			log.Printf("%s watcher skip reason=masked_private event=%s", a.cfg.AgentID, event.ev.ID)
			continue // skip 6
		}
		if !event.end.After(now) {
			continue // skip 7: already ended
		}
		if event.start.After(now.AddDate(0, 0, 30)) {
			continue // skip 8: beyond the writer's insert horizon (cannot happen in a 4-day scan; assert anyway)
		}
		if w.isOffsite(event, loc) {
			offsite = append(offsite, event)
		}
	}

	// Travel blocks are reconciled FIRST so a reattach (§7.5(3)) claims its
	// (meeting, side) and the creation pass does not also insert a fresh
	// block for the same side in the same tick.
	var mutations []watchMutation
	claimed := make(map[string]bool) // meeting.ev.ID + "|" + side claimed by a reattach
	for _, block := range grammarBlocks {
		if mutation, ok, claim := w.planBlock(block, offsite, allDay, anchors, travelBlocks, neighbors, now, loc, degraded); ok {
			mutations = append(mutations, mutation)
			if claim != "" {
				claimed[claim] = true
			}
		}
	}
	for _, meeting := range offsite {
		mutations = append(mutations, w.planMeeting(meeting, allDay, anchors, neighbors, travelBlocks, claimed, now, loc)...)
	}
	return mutations
}

// isOffsite implements §7.2: yellow category OR a non-empty location that is
// neither the office nor virtual. Event-specific durable location rules can
// infer a physical location from context before the plain location test.
func (w *travelWatcher) isOffsite(event watchEvent, loc *time.Location) bool {
	if w.hasYellowCategory(event.ev.Categories) {
		return true
	}
	if w.inferredFaceToFaceLocation(event, loc) != "" {
		return true
	}
	location := strings.TrimSpace(event.ev.Location)
	if location == "" {
		return false
	}
	knowledge := w.agent.knowledge
	return !knowledge.IsOffice(location) && !knowledge.IsVirtual(location)
}

func (w *travelWatcher) hasYellowCategory(categories []string) bool {
	configured := strings.TrimSpace(w.agent.cfg.OffsiteCategory)
	for _, category := range categories {
		if configured != "" && strings.EqualFold(strings.TrimSpace(category), configured) {
			return true
		}
		if strings.Contains(strings.ToLower(category), "yellow") {
			return true
		}
	}
	return false
}

// estimateLocation is the location string fed to the estimator for an
// offsite meeting: virtual locations (offsite only via yellow) and empty
// locations degrade to "" (no venue match => default estimate, dest
// "offsite").
func (w *travelWatcher) estimateLocation(event watchEvent, loc *time.Location) string {
	if inferred := w.inferredFaceToFaceLocation(event, loc); inferred != "" {
		return inferred
	}
	location := strings.TrimSpace(event.ev.Location)
	if location == "" || w.agent.knowledge.IsVirtual(location) {
		return ""
	}
	return location
}

// blockExists implements the §7.3 ownership-blind adjacency test: a
// human-created or other-agent travel event in the slot counts as existing.
func blockExists(side string, meeting watchEvent, travelBlocks []watchEvent) bool {
	for _, block := range travelBlocks {
		if block.ev.ID == meeting.ev.ID {
			continue
		}
		if sideAdjacent(side, meeting.start, meeting.end, block) {
			return true
		}
	}
	return false
}

// sideAdjacent reports whether travel block T satisfies the side's adjacency
// to bounds [start, end): before: T.end in [start-20m, start+5m]; after:
// T.start in [end-5m, end+20m].
func sideAdjacent(side string, start, end time.Time, block watchEvent) bool {
	if side == "before" {
		return !block.end.Before(start.Add(-20*time.Minute)) && !block.end.After(start.Add(5*time.Minute))
	}
	return !block.start.Before(end.Add(-5*time.Minute)) && !block.start.After(end.Add(20*time.Minute))
}

// planMeeting emits missing-block inserts for one offsite meeting. Sides
// claimed by a same-tick reattach are skipped.
func (w *travelWatcher) planMeeting(meeting watchEvent, allDay, anchors, neighbors, travelBlocks []watchEvent, claimed map[string]bool, now time.Time, loc *time.Location) []watchMutation {
	a := w.agent
	var out []watchMutation
	location := w.estimateLocation(meeting, loc)

	for _, side := range []string{"before", "after"} {
		if side == "before" && !meeting.start.After(now) {
			continue
		}
		if claimed[meeting.ev.ID+"|"+side] {
			continue
		}
		if blockExists(side, meeting, travelBlocks) {
			continue
		}
		parentKey := meeting.ev.ID + "|" + formatLA(meeting.start) + "|" + side
		if w.humanRemoved[parentKey] {
			continue
		}
		estimate, err := w.estimateForSide(side, meeting, allDay, anchors, location, loc)
		if err != nil {
			a.metrics.IncCounter("watch_travel_skipped")
			log.Printf("%s watcher skip reason=no_origin event=%s side=%s", a.cfg.AgentID, meeting.ev.ID, side)
			return nil // skip this meeting this tick
		}
		blockMinutes := travelknowledge.BlockMinutes(estimate)
		interval, ok, reason := shrinkInterval(side, meeting, blockMinutes, neighbors, now, loc)
		if !ok {
			a.metrics.IncCounter("watch_travel_skipped")
			log.Printf("%s watcher skip reason=%s event=%s side=%s", a.cfg.AgentID, reason, meeting.ev.ID, side)
			continue
		}
		w.maybeProposeCompressedTravel(side, meeting, neighbors, blockMinutes, interval, loc)
		key := "schedw-" + idempotencyHash(strings.Join([]string{
			meeting.ev.ID, formatLA(meeting.start), formatLA(meeting.end), side,
			formatLA(interval.Start), formatLA(interval.End),
		}, "|"))
		if w.succeededInserts[key] {
			// We inserted this exact block and it is gone: Joel deleted it.
			w.humanRemoved[parentKey] = true
			a.metrics.IncCounter("watch_travel_human_removed")
			log.Printf("%s watcher: travel block removed by a human; will not recreate parent=%s side=%s key=%s", a.cfg.AgentID, meeting.ev.ID, side, key)
			continue
		}
		if w.backoff[key] {
			continue
		}
		dest := destinationLabel(estimate, location)
		parking := strings.TrimSpace(estimate.Parking)
		if parking == "" {
			parking = "unknown"
		}
		sideWord := "for"
		if side == "after" {
			sideWord = "return"
		}
		travelLocation := travelBlockLocation(estimate, location, sideWord)
		payload := travelInsertRequest(dest, travelLocation, parking, meeting.ev.ID, formatLA(meeting.start), meeting.start, interval.Start, interval.End, sideWord)
		out = append(out, watchMutation{kind: "insert", key: key, payload: payload, parentKey: parentKey})
	}
	return out
}

// shrinkInterval computes the desired travel interval for a side and shrinks
// it against neighbors (§7.3): overlapping busy events raise the before
// block's start (or lower the after block's end); an event covering the
// meeting boundary blocks the side. The surviving length is rounded DOWN to a
// 5-minute multiple, anchored at the meeting boundary; under 10 minutes the
// side is skipped.
func shrinkInterval(side string, meeting watchEvent, blockMinutes int, neighbors []watchEvent, now time.Time, loc *time.Location) (Interval, bool, string) {
	block := time.Duration(blockMinutes) * time.Minute
	var interval Interval
	if side == "before" {
		interval = Interval{Start: meeting.start.Add(-block), End: meeting.start}
	} else {
		interval = Interval{Start: meeting.end, End: meeting.end.Add(block)}
	}
	for _, neighbor := range neighbors {
		if neighbor.ev.ID == meeting.ev.ID {
			continue
		}
		if !intervalsOverlap(interval.Start, interval.End, neighbor.start, neighbor.end) {
			continue
		}
		if side == "before" {
			if neighbor.end.Before(meeting.start) {
				if neighbor.end.After(interval.Start) {
					interval.Start = neighbor.end
				}
			} else {
				return Interval{}, false, "adjacent_busy"
			}
		} else {
			if neighbor.start.After(meeting.end) {
				if neighbor.start.Before(interval.End) {
					interval.End = neighbor.start
				}
			} else {
				return Interval{}, false, "adjacent_busy"
			}
		}
	}
	if side == "before" {
		if lead := now.Add(time.Minute); interval.Start.Before(lead) {
			interval.Start = lead
		}
		boundary := meeting.start.In(loc)
		floor := time.Date(boundary.Year(), boundary.Month(), boundary.Day(), 6, 0, 0, 0, loc)
		if interval.Start.Before(floor) {
			interval.Start = floor
		}
	} else {
		boundary := meeting.end.In(loc)
		ceiling := time.Date(boundary.Year(), boundary.Month(), boundary.Day(), 22, 0, 0, 0, loc)
		if interval.End.After(ceiling) {
			interval.End = ceiling
		}
	}
	length := interval.End.Sub(interval.Start)
	rounded := length - (length % (5 * time.Minute))
	if rounded < 10*time.Minute {
		return Interval{}, false, "too_short"
	}
	if side == "before" {
		interval.Start = interval.End.Add(-rounded)
	} else {
		interval.End = interval.Start.Add(rounded)
	}
	return interval, true, ""
}

// planBlock implements §7.5 for one grammar-matching travel block T:
// attached => nothing; anchored to ANY busy meeting => leave alone (protects
// blocks around scheduler-booked offsite HOLDS, which carry no
// location/category); parent really moved (no offsite meeting at the grammar
// time) and exactly one side-lacking offsite meeting => reattach; otherwise
// orphan => cancel (suppressed on truncated ticks). The returned claim
// ("<meeting id>|<side>") tells the creation pass a reattach already serves
// that side this tick.
//
// A parent whose start is unchanged but whose other bound moved (so the
// grammar time still matches a meeting while adjacency fails) is NOT treated
// as moved: the stale block is cancelled and the creation pass issues a fresh
// insert under a new interval-embedding key (§7.3, review should-fix S7).
func (w *travelWatcher) planBlock(block watchEvent, offsite, allDay, anchors, travelBlocks, neighbors []watchEvent, now time.Time, loc *time.Location, degraded bool) (watchMutation, bool, string) {
	a := w.agent
	summary := strings.TrimSpace(block.ev.Summary)
	match := travelSummaryRE.FindStringSubmatch(summary)
	if match == nil {
		return watchMutation{}, false, ""
	}
	side := "before"
	if match[2] == "return" {
		side = "after"
	}
	grammarHHMM := match[3]
	blockDate := localDateStart(block.start, loc)

	// Step 1: still attached?
	var sameStart []watchEvent
	for _, meeting := range offsite {
		if blockParentMatches(side, grammarHHMM, meeting, blockDate, loc) {
			sameStart = append(sameStart, meeting)
		}
	}
	if len(sameStart) > 1 {
		a.metrics.IncCounter("watch_travel_ambiguous")
		log.Printf("%s watcher: ambiguous parent (multiple offsite meetings at %s) block=%s", a.cfg.AgentID, grammarHHMM, block.ev.ID)
		return watchMutation{}, false, ""
	}
	if len(sameStart) == 1 && sideAdjacent(side, sameStart[0].start, sameStart[0].end, block) {
		w.maybeProposeAttachedCompressedTravel(side, sameStart[0], block, allDay, anchors, neighbors, loc)
		if mutation, ok := w.planAttachedBlockRepair(block, sameStart[0], side, allDay, anchors, loc); ok {
			return mutation, true, sameStart[0].ev.ID + "|" + side
		}
		return watchMutation{}, false, ""
	}

	// Step 2: anchored to anything busy (not just offsite)?
	for _, anchor := range anchors {
		if sideAdjacent(side, anchor.start, anchor.end, block) {
			return watchMutation{}, false, ""
		}
	}

	if w.notOwned[block.ev.ID] {
		return watchMutation{}, false, ""
	}
	entryID := block.entryID()
	if entryID == "" {
		a.metrics.IncCounter("watch_travel_no_entry_id")
		log.Printf("%s watcher: travel block lacks entry_id; mutation skipped block=%s", a.cfg.AgentID, block.ev.ID)
		return watchMutation{}, false, ""
	}

	// Step 3: reattach (moved parent): only when NO offsite meeting still
	// starts at the grammar time on this date.
	if len(sameStart) == 0 {
		var candidates []watchEvent
		for _, meeting := range offsite {
			if !localDateStart(meeting.start.In(loc), loc).Equal(blockDate) {
				continue
			}
			if !blockExists(side, meeting, travelBlocks) {
				candidates = append(candidates, meeting)
			}
		}
		if len(candidates) > 1 {
			a.metrics.IncCounter("watch_travel_ambiguous")
			log.Printf("%s watcher: ambiguous reattach (multiple side-lacking offsite meetings) block=%s", a.cfg.AgentID, block.ev.ID)
			return watchMutation{}, false, ""
		}
		if len(candidates) == 1 {
			meeting := candidates[0]
			location := w.estimateLocation(meeting, loc)
			estimate, err := w.estimateForSide(side, meeting, allDay, anchors, location, loc)
			if err == nil {
				if interval, ok, _ := shrinkInterval(side, meeting, travelknowledge.BlockMinutes(estimate), neighbors, now, loc); ok {
					key := "schedw-move-" + idempotencyHash(strings.Join([]string{
						block.ev.ID, meeting.ev.ID, formatLA(meeting.start), formatLA(meeting.end), side,
						formatLA(interval.Start), formatLA(interval.End),
					}, "|"))
					if w.backoff[key] {
						return watchMutation{}, false, ""
					}
					dest := destinationLabel(estimate, location)
					parking := strings.TrimSpace(estimate.Parking)
					if parking == "" {
						parking = "unknown"
					}
					sideWord := "for"
					if side == "after" {
						sideWord = "return"
					}
					travelLocation := travelBlockLocation(estimate, location, sideWord)
					labelTime := meeting.start
					if side == "after" {
						labelTime = meeting.end
					}
					newSummary := travelBlockSummary(dest, sideWord, labelTime, loc)
					description := travelDescription(meeting.ev.ID, formatLA(meeting.start), dest, travelLocation, parking)
					payload := outlookcalendarwrite.Request{
						Action:     "event-patch",
						CalendarID: "default",
						EventID:    entryID,
						Event: outlookcalendarwrite.EventInput{
							Summary:     &newSummary,
							Description: &description,
							Location:    optionalSchedulerString(travelLocation),
							Start:       writerDateTime(formatLA(interval.Start)),
							End:         writerDateTime(formatLA(interval.End)),
						},
					}
					claim := meeting.ev.ID + "|" + side
					return watchMutation{kind: "move", key: key, payload: payload, eventID: block.ev.ID}, true, claim
				}
			}
			// Recomputed side unfittable (or no origin): fall through to cancel.
		}
	}

	// Step 4: orphan => cancel (never during a degraded tick: missing events
	// must not look like orphaned parents).
	if degraded {
		return watchMutation{}, false, ""
	}
	key := "schedw-cancel-" + idempotencyHash(block.ev.ID+"|"+formatLA(block.start))
	if w.backoff[key] {
		return watchMutation{}, false, ""
	}
	return watchMutation{kind: "cancel", key: key, payload: cancelPatchRequest(entryID), eventID: block.ev.ID}, true, ""
}

func (w *travelWatcher) planAttachedBlockRepair(block, meeting watchEvent, side string, allDay, anchors []watchEvent, loc *time.Location) (watchMutation, bool) {
	if w.notOwned[block.ev.ID] {
		return watchMutation{}, false
	}
	entryID := block.entryID()
	if entryID == "" {
		log.Printf("%s watcher: attached travel block lacks entry_id; repair skipped block=%s", w.agent.cfg.AgentID, block.ev.ID)
		return watchMutation{}, false
	}
	location := w.estimateLocation(meeting, loc)
	estimate, err := w.estimateForSide(side, meeting, allDay, anchors, location, loc)
	if err != nil {
		w.agent.metrics.IncCounter("watch_travel_skipped")
		log.Printf("%s watcher repair skip reason=no_origin event=%s side=%s", w.agent.cfg.AgentID, meeting.ev.ID, side)
		return watchMutation{}, false
	}
	dest := destinationLabel(estimate, location)
	parking := strings.TrimSpace(estimate.Parking)
	if parking == "" {
		parking = "unknown"
	}
	sideWord := "for"
	labelTime := meeting.start
	if side == "after" {
		sideWord = "return"
		labelTime = meeting.end
	}
	travelLocation := travelBlockLocation(estimate, location, sideWord)
	wantSummary := travelBlockSummary(dest, sideWord, labelTime, loc)
	wantDescription := travelDescription(meeting.ev.ID, formatLA(meeting.start), dest, travelLocation, parking)

	currentSummary := strings.TrimSpace(block.ev.Summary)
	currentLocation := strings.TrimSpace(block.ev.Location)
	if currentSummary == wantSummary && currentLocation == travelLocation {
		return watchMutation{}, false
	}

	payload := outlookcalendarwrite.Request{
		Action:     "event-patch",
		CalendarID: "default",
		EventID:    entryID,
		Event: outlookcalendarwrite.EventInput{
			Description: &wantDescription,
			Start:       writerDateTime(formatLA(block.start)),
			End:         writerDateTime(formatLA(block.end)),
		},
	}
	if currentSummary != wantSummary {
		payload.Event.Summary = &wantSummary
	}
	if currentLocation != travelLocation {
		locationCopy := travelLocation
		payload.Event.Location = &locationCopy
	}
	key := "schedw-repair-" + idempotencyHash(strings.Join([]string{
		block.ev.ID, meeting.ev.ID, formatLA(meeting.start), formatLA(meeting.end), side,
		currentSummary, currentLocation, wantSummary, travelLocation,
	}, "|"))
	if w.backoff[key] {
		return watchMutation{}, false
	}
	return watchMutation{kind: "repair", key: key, payload: payload, eventID: block.ev.ID}, true
}

// execute applies the tick's mutations under the per-tick cap, tracking
// backoff, not_owned denylisting, and human-deletion success memory.
func (w *travelWatcher) execute(ctx context.Context, session upstreamSession, mutations []watchMutation) {
	a := w.agent
	performed := 0
	for _, mutation := range mutations {
		if performed >= maxWatchMutationsPerTick {
			a.metrics.IncCounter("watch_mutations_deferred")
			continue
		}
		performed++
		resp, err := a.writeMutationWithKey(ctx, session, mutation.key, mutation.payload, mutation.key)
		if err != nil {
			w.recordFailure(mutation.key)
			log.Printf("%s watcher mutation failed kind=%s key=%s err=%v", a.cfg.AgentID, mutation.kind, mutation.key, err)
			continue
		}
		if strings.TrimSpace(resp.Error) != "" {
			if resp.ErrorCode == ErrorNotOwned && mutation.eventID != "" {
				w.notOwned[mutation.eventID] = true
				a.metrics.IncCounter("watch_travel_not_owned")
				log.Printf("%s watcher mutation refused not_owned kind=%s event=%s", a.cfg.AgentID, mutation.kind, mutation.eventID)
				continue
			}
			w.recordFailure(mutation.key)
			log.Printf("%s watcher mutation refused kind=%s key=%s code=%s err=%s", a.cfg.AgentID, mutation.kind, mutation.key, resp.ErrorCode, resp.Error)
			continue
		}
		delete(w.failures, mutation.key)
		switch mutation.kind {
		case "insert":
			a.metrics.IncCounter("watch_travel_created")
			// Dry-run would_write responses are treated as success for
			// backoff purposes but must NOT arm the human-deletion detector:
			// nothing was written, so the block will legitimately read as
			// missing next tick.
			if !resp.DryRun {
				w.succeededInserts[mutation.key] = true
			}
		case "move":
			a.metrics.IncCounter("watch_travel_moved")
		case "cancel":
			a.metrics.IncCounter("watch_travel_cancelled")
		case "repair":
			a.metrics.IncCounter("watch_travel_repaired")
		}
	}
}

func (w *travelWatcher) recordFailure(key string) {
	w.failures[key]++
	if w.failures[key] >= watchBackoffFailures && !w.backoff[key] {
		w.backoff[key] = true
		w.agent.metrics.IncCounter("watch_travel_backoff")
		log.Printf("%s watcher backing off key=%s after %d consecutive failures", w.agent.cfg.AgentID, key, watchBackoffFailures)
	}
}
