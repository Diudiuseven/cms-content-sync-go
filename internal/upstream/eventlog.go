package upstream

import (
	"sort"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

// The source keeps an append-only event log so that historical events can be
// replayed after an interruption. The log is the source of truth for what
// happened; the contents map is a derived view.

// logEventsAfter returns all logged events with seq > after, ascending.
func (s *Source) logEventsAfter(after int64) []*model.Event {
	// events is stored on the source via the eventLog field.
	events := s.eventLog
	out := make([]*model.Event, 0)
	for _, e := range events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// appendEvent adds an event to the log.
func (s *Source) appendEvent(e *model.Event) {
	s.eventLog = append(s.eventLog, e)
}
