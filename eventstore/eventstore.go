package eventstore

import (
	"database/sql"

	"github.com/sorintlab/sircles/common"
	"github.com/sorintlab/sircles/db"
	slog "github.com/sorintlab/sircles/log"
	"github.com/sorintlab/sircles/util"

	sq "github.com/Masterminds/squirrel"
	"github.com/pkg/errors"
)

var log = slog.S()

const (
	eventStoreExclusiveLock = iota
)

var (
	// Use postgresql $ placeholder. It'll be converted to ? from the provided db functions
	sb = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

	eventSelect         = sb.Select("id", "sequencenumber", "eventtype", "category", "streamid", "timestamp", "version", "data", "metadata").From("event")
	eventInsert         = sb.Insert("event").Columns("id", "eventtype", "category", "streamid", "timestamp", "version", "data", "metadata")
	streamVersionSelect = sb.Select("category", "streamid", "version").From("streamversion")
	streamVersionInsert = sb.Insert("streamversion").Columns("category", "streamid", "version")
)

type EventStore struct {
	tx *db.Tx
	tg common.TimeGenerator
}

func NewEventStore(tx *db.Tx) *EventStore {
	return &EventStore{
		tx: tx,
		tg: common.DefaultTimeGenerator{},
	}
}

func (s *EventStore) SetTimeGenerator(tg common.TimeGenerator) {
	s.tg = tg
}

func scanEvent(rows *sql.Rows) (*StoredEvent, error) {
	e := StoredEvent{}
	var data, metaData []byte
	// To make sqlite3 happy
	var eventType, category string
	fields := []interface{}{&e.ID, &e.SequenceNumber, &eventType, &category, &e.StreamID, &e.Timestamp, &e.Version, &data, &metaData}
	if err := rows.Scan(fields...); err != nil {
		return nil, errors.Wrap(err, "error scanning event")
	}
	e.EventType = EventType(eventType)
	e.Category = category
	e.Data = data
	e.MetaData = metaData
	return &e, nil
}

func scanEvents(rows *sql.Rows) ([]*StoredEvent, error) {
	events := []*StoredEvent{}
	for rows.Next() {
		m, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, m)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.WithStack(err)
	}
	return events, nil
}

func (s *EventStore) insertEvent(tx *db.Tx, event *StoredEvent) error {
	q, args, err := eventInsert.Values(event.ID, event.EventType, event.Category, event.StreamID, event.Timestamp, event.Version, event.Data, event.MetaData).ToSql()
	if err != nil {
		return errors.Wrap(err, "failed to build query")
	}

	err = tx.Do(func(tx *db.WrappedTx) error {
		_, err = tx.Exec(q, args...)
		return err
	})
	if err != nil {
		return errors.WithMessage(err, "failed to execute query")
	}
	return nil
}

func (s *EventStore) insertStreamVersion(tx *db.Tx, av *StreamVersion) error {
	q, args, err := streamVersionInsert.Values(av.Category, av.StreamID, av.Version).ToSql()
	if err != nil {
		return errors.Wrap(err, "failed to build query")
	}

	err = tx.Do(func(tx *db.WrappedTx) error {
		// poor man insert or update...
		if _, err := tx.Exec("delete from streamversion where streamid = $1", av.StreamID); err != nil {
			return errors.WithMessage(err, "failed to delete streamversion")
		}
		_, err = tx.Exec(q, args...)
		return err
	})
	if err != nil {
		return errors.WithMessage(err, "failed to execute query")
	}
	return nil
}

func (s *EventStore) LastSequenceNumber() (int64, error) {

	// Get last sequence
	sb := eventSelect.OrderBy("sequencenumber DESC").Limit(1)

	q, args, err := sb.ToSql()
	if err != nil {
		return 0, errors.Wrap(err, "failed to build query")
	}

	var es []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.Wrap(err, "failed to execute query")
		}
		es, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return 0, err
	}

	if len(es) > 0 {
		return es[0].SequenceNumber, nil
	}
	return int64(0), nil
}

func (s *EventStore) WriteEvents(eventsData []*EventData, category string, streamID string, version int64) ([]*StoredEvent, error) {
	if len(eventsData) == 0 {
		return nil, nil
	}

	sb := sb.Select("category", "version").From("streamversion").Where(sq.Eq{"streamid": streamID})
	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var curVersion int64
	var at string
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		err := tx.QueryRow(q, args...).Scan(&at, &curVersion)
		if err != nil && err != sql.ErrNoRows {
			return errors.WithMessage(err, "failed to execute query")
		}
		return nil
	})

	// optimistic locking: check current version with expected version.
	// NOTE This doesn't catch concurrent transactions updating the same
	// stream, this is catched by unique constraints on (category,
	// streamID, version).
	if curVersion != version {
		return nil, errors.Errorf("current version %d different than provided version %d", curVersion, version)
	}

	if version != 0 && category != at {
		return nil, errors.Errorf("stream in version has different category")
	}

	prevVersion := version

	// write the events

	events := make([]*StoredEvent, len(eventsData))

	for i, ed := range eventsData {
		version++

		e := &StoredEvent{
			ID:        ed.ID,
			EventType: ed.EventType,
			Category:  category,
			StreamID:  streamID,
			Data:      ed.Data,
			MetaData:  ed.MetaData,

			Timestamp: s.tg.Now(),
			Version:   version,
		}
		events[i] = e
	}

	// take exlusive lock.
	// In this way we'll commit ordered (but not gapless) sequence numbers and avoid
	// races where a lower sequence number is committed after an higher one
	// causing handlers relying to the sequence number to lose these events.
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		_, err = tx.Exec("select pg_advisory_xact_lock($1)", eventStoreExclusiveLock)
		return err
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to take exlusive lock")
	}
	for _, e := range events {
		if err := s.insertEvent(s.tx, e); err != nil {
			return nil, err
		}
	}

	// Update the stream version
	if version == prevVersion {
		return nil, nil
	}
	log.Debugf("updating stream %s to version: %d", streamID, version)
	if err := s.insertStreamVersion(s.tx, &StreamVersion{Category: category, StreamID: streamID, Version: version}); err != nil {
		return nil, err
	}

	return events, nil
}

func (s *EventStore) RestoreEvents(events []*StoredEvent) error {
	versions := map[string]*StreamVersion{}

	// Write the events
	for _, e := range events {
		if err := s.insertEvent(s.tx, e); err != nil {
			return err
		}

		versions[e.StreamID] = &StreamVersion{
			Category: e.Category,
			StreamID: e.StreamID,
			Version:  e.Version,
		}
	}

	// Update the stream version
	for _, av := range versions {
		if err := s.insertStreamVersion(s.tx, av); err != nil {
			return err
		}
	}

	return nil
}

func (s *EventStore) GetEvent(id *util.ID) (*StoredEvent, error) {
	sb := eventSelect.Where(sq.Eq{"id": id})

	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var events []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.Wrap(err, "failed to execute query")
		}
		events, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	if len(events) > 1 {
		return nil, errors.Errorf("too many events. This shouldn't happen!")
	}
	return events[0], nil
}

func (s *EventStore) GetAllEvents(start int64, count uint64) ([]*StoredEvent, error) {
	if count < 1 {
		return []*StoredEvent{}, nil
	}

	sb := eventSelect.Where(sq.GtOrEq{"sequencenumber": start}).OrderBy("sequencenumber ASC").Limit(count)

	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var events []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.WithMessage(err, "failed to execute query")
		}
		events, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *EventStore) GetEventsByCategory(category string, start int64, count uint64) ([]*StoredEvent, error) {
	if count < 1 {
		return []*StoredEvent{}, nil
	}

	sb := eventSelect.Where(sq.And{sq.Eq{"category": category}, sq.GtOrEq{"sequencenumber": start}}).OrderBy("sequencenumber ASC").Limit(count)

	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var events []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.WithMessage(err, "failed to execute query")
		}
		events, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *EventStore) GetEvents(streamID string, startVersion int64, count uint64) ([]*StoredEvent, error) {
	if count < 1 {
		return []*StoredEvent{}, nil
	}

	sb := eventSelect.Where(sq.And{sq.Eq{"streamid": streamID}, sq.GtOrEq{"version": startVersion}}).OrderBy("version ASC").Limit(count)

	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var events []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.WithMessage(err, "failed to execute query")
		}
		events, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *EventStore) GetLastEvent(streamID string) (*StoredEvent, error) {
	sb := eventSelect.Where(sq.And{sq.Eq{"streamid": streamID}}).OrderBy("version DESC").Limit(1)

	q, args, err := sb.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build query")
	}

	var events []*StoredEvent
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return errors.WithMessage(err, "failed to execute query")
		}
		events, err = scanEvents(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	return events[0], nil
}

func (s *EventStore) StreamVersion(streamID string) (int64, error) {
	sb := sb.Select("category", "version").From("streamversion").Where(sq.Eq{"streamID": streamID})
	q, args, err := sb.ToSql()
	if err != nil {
		return 0, errors.Wrap(err, "failed to build query")
	}

	var curVersion int64
	var at string
	err = s.tx.Do(func(tx *db.WrappedTx) error {
		err := tx.QueryRow(q, args...).Scan(&at, &curVersion)
		if err != nil && err != sql.ErrNoRows {
			return errors.WithMessage(err, "failed to execute query")
		}
		return nil
	})
	return curVersion, nil
}
