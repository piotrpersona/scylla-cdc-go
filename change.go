package scylla_cdc

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/gocql/gocql"
)

type OperationType int8

const (
	PreImage                  OperationType = 0
	Update                                  = 1
	Insert                                  = 2
	RowDelete                               = 3
	PartitionDelete                         = 4
	RangeDeleteStartInclusive               = 5
	RangeDeleteStartExclusive               = 6
	RangeDeleteEndInclusive                 = 7
	RangeDeleteEndExclusive                 = 8
	PostImage                               = 9
)

func (ot OperationType) String() string {
	switch ot {
	case PreImage:
		return "PREIMAGE"
	case Update:
		return "UPDATE"
	case Insert:
		return "INSERT"
	case RowDelete:
		return "ROW_DELETE"
	case PartitionDelete:
		return "PARTITION_DELETE"
	case RangeDeleteStartInclusive:
		return "RANGE_DELETE_START_INCLUSIVE"
	case RangeDeleteStartExclusive:
		return "RANGE_DELETE_START_EXCLUSIVE"
	case RangeDeleteEndInclusive:
		return "RANGE_DELETE_END_INCLUSIVE"
	case RangeDeleteEndExclusive:
		return "RANGE_DELETE_END_EXCLUSIVE"
	case PostImage:
		return "POSTIMAGE"
	default:
		return "(wrong OperationType)"
	}
}

type Change struct {
	StreamID  []byte
	Time      gocql.UUID
	Preimage  []*ChangeRow
	Delta     []*ChangeRow
	Postimage []*ChangeRow
}

// GetCassandraTimestamp returns a timestamp of the operation
// suitable to put as a TIMESTAMP parameter to a DML statement
// (INSERT, UPDATE, DELETE)
func (c *Change) GetCassandraTimestamp() int64 {
	return timeuuidToTimestamp(c.Time)
}

// ChangeRow corresponds to a single row from the cdc log
type ChangeRow struct {
	data    map[string]interface{}
	cdcCols cdcChangeCols

	colInfos []gocql.ColumnInfo
}

type cdcStreamCols struct {
	streamID []byte
	time     gocql.UUID
}

type cdcChangeCols struct {
	batchSeqNo int32
	operation  int8
	ttl        int64
	endOfBatch bool
}

// GetOperation returns the type of operation this change represents
func (c *ChangeRow) GetOperation() OperationType {
	return OperationType(c.cdcCols.operation)
}

// GetTTL returns 0 if TTL was not set for this operation
func (c *ChangeRow) GetTTL() int64 {
	return c.cdcCols.ttl
}

// GetValue returns value that was assigned to this specific column
func (c *ChangeRow) GetValue(columnName string) (interface{}, bool) {
	v, ok := c.data[columnName]
	return v, ok
}

// IsDeleted returns a boolean indicating if given column was set to null.
// This only works for clustering columns.
func (c *ChangeRow) IsDeleted(columnName string) (bool, bool) {
	v, ok := c.data["cdc$deleted_"+columnName]
	if !ok {
		return false, false
	}
	return v.(bool), true
}

// GetDeletedElements returns which elements were deleted from the non-atomic column.
// This function works only for non-atomic columns
func (c *ChangeRow) GetDeletedElements(columnName string) (interface{}, bool) {
	v, ok := c.data["cdc$deleted_elements_"+columnName]
	return v, ok
}

// Columns returns information about data columns in the cdc log table (without those with "cdc$" prefix)
func (c *ChangeRow) Columns() []gocql.ColumnInfo {
	return c.colInfos
}

func (c *ChangeRow) String() string {
	// TODO: This doesn't work correctly because null columns are not inserted
	// to the map

	var b strings.Builder
	b.WriteString(OperationType(c.cdcCols.operation).String())
	b.WriteString(" ")
	b.WriteString(strconv.FormatInt(c.cdcCols.ttl, 10))
	b.WriteString(" -> {")
	first := true

	// Sort field names
	sortedFieldNames := make([]string, 0, len(c.data))
	for k := range c.data {
		if strings.HasPrefix(k, "cdc$deleted_") {
			continue
		}
		sortedFieldNames = append(sortedFieldNames, k)
	}

	// Copy field names, and included cdc$deleted_ columns
	sort.Strings(sortedFieldNames)
	ks := make([]string, 0, len(c.data))
	for _, k := range sortedFieldNames {
		ks = append(ks, k)
		deleted := "cdc$deleted_" + k
		deletedElements := "cdc$deleted_elements_" + k
		if _, hasDeleted := c.data[deleted]; hasDeleted {
			ks = append(ks, deleted)
		}
		if _, hasDeletedElements := c.data[deletedElements]; hasDeletedElements {
			ks = append(ks, deletedElements)
		}
	}

	// Print columns in order
	for _, k := range ks {
		v, present := c.data[k]
		if !first {
			b.WriteString(" ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(":")
		if strings.HasPrefix(k, "cdc$deleted_") {
			b.WriteString(fmt.Sprintf("%v", v))
		} else {
			if present {
				b.WriteString(fmt.Sprintf("%v", v))
			} else {
				b.WriteString("nil")
			}
		}
	}
	b.WriteString("}")
	return b.String()
}

type CreateChangeConsumerInput struct {
	TableName string
	streamIDs []StreamID
}

type ChangeConsumerFactory interface {
	CreateChangeConsumer(input CreateChangeConsumerInput) (ChangeConsumer, error)
}

type ChangeConsumer interface {
	End()
	Consume(change Change) error
}

func MakeChangeConsumerFactoryFromFunc(f ChangeConsumerFunc) ChangeConsumerFactory {
	return &changeConsumerFuncInstanceFactory{f}
}

type changeConsumerFuncInstanceFactory struct {
	f ChangeConsumerFunc
}

func (ccfif *changeConsumerFuncInstanceFactory) CreateChangeConsumer(input CreateChangeConsumerInput) (ChangeConsumer, error) {
	return &changeConsumerFuncInstance{
		tableName: input.TableName,
		f:         ccfif.f,
	}, nil
}

type changeConsumerFuncInstance struct {
	tableName string
	f         ChangeConsumerFunc
}

func (ccfi *changeConsumerFuncInstance) End() {} // TODO: Snapshot here?
func (ccfi *changeConsumerFuncInstance) Consume(change Change) error {
	return ccfi.f(ccfi.tableName, change)
}

type ChangeConsumerFunc func(tableName string, change Change) error

// An adapter over gocql.Iterator
type changeRowIterator struct {
	iter         *gocql.Iter
	columnValues []interface{}

	cdcStreamCols cdcStreamCols
	cdcChangeCols cdcChangeCols

	colInfos []gocql.ColumnInfo
}

func newChangeRowIterator(iter *gocql.Iter) *changeRowIterator {
	// TODO: Check how costly is the reflection here
	// We could amortize the cost by preparing the dataFields only at the
	// beginning of the iteration, and change them only if the fields
	// have changed
	// This possibility should be looked into

	allCols := iter.Columns()

	ci := &changeRowIterator{
		iter:         iter,
		columnValues: make([]interface{}, 0, len(allCols)),
		colInfos:     make([]gocql.ColumnInfo, 0, len(allCols)),
	}

	for _, col := range allCols {
		if !strings.HasPrefix(col.Name, "cdc$") {
			ci.colInfos = append(ci.colInfos, col)
		}

		if tupTyp, ok := col.TypeInfo.(gocql.TupleTypeInfo); ok {
			for _, el := range tupTyp.Elems {
				ci.columnValues = append(ci.columnValues, reflect.New(reflect.TypeOf(el.New())).Interface())
			}
		} else {
			var cval interface{}
			switch col.Name {
			case "cdc$stream_id":
				cval = &ci.cdcStreamCols.streamID
			case "cdc$time":
				cval = &ci.cdcStreamCols.time
			case "cdc$batch_seq_no":
				cval = &ci.cdcChangeCols.batchSeqNo
			case "cdc$ttl":
				cval = &ci.cdcChangeCols.ttl
			case "cdc$operation":
				cval = &ci.cdcChangeCols.operation
			case "cdc$end_of_batch":
				cval = &ci.cdcChangeCols.endOfBatch

			default:
				if !strings.HasPrefix(col.Name, "cdc$deleted_") {
					// All non-cdc fields should be nullable
					cval = reflect.New(reflect.TypeOf(col.TypeInfo.New())).Interface()
				} else {
					cval = col.TypeInfo.New()
				}
			}
			ci.columnValues = append(ci.columnValues, cval)
		}
	}

	return ci
}

func (ci *changeRowIterator) Next() (cdcStreamCols, *ChangeRow) {
	if !ci.iter.Scan(ci.columnValues...) {
		return cdcStreamCols{}, nil
	}

	// Make a copy so that the Change object can be used safely after Next() is called again
	// TODO: Maybe we can omit copying here? We could re-use a single map
	// But it would require entrusting the user with performing a necessary copy
	// if they want to preserve data across Next() calls
	// TODO: Can we design an interface which scans into user-provided struct?
	change := &ChangeRow{
		data:     make(map[string]interface{}, len(ci.columnValues)-6),
		cdcCols:  ci.cdcChangeCols,
		colInfos: ci.colInfos,
	}

	pos := 0
	for _, col := range ci.iter.Columns() {
		// TODO: Optimize
		if strings.HasPrefix(col.Name, "cdc$") && !strings.HasPrefix(col.Name, "cdc$deleted_") {
			pos++
			continue
		}

		if tupTyp, ok := col.TypeInfo.(gocql.TupleTypeInfo); ok {
			// We deviate from gocql's convention here - we represent a tuple
			// as an []interface{}, we don't keep a separate column for each
			// tuple element.
			// This was made in order to avoid confusion with respect to
			// the cdc log table - if we split tuple v into v[0], v[1], ...,
			// we would also have to artificially split cdc$deleted_v
			// into cdc$deleted_v[0], cdc$deleted_v[1]...

			// TODO: Check if the tuple was null
			tupLen := len(tupTyp.Elems)
			v := make([]interface{}, tupLen)
			copy(v, ci.columnValues[pos:pos+tupLen])

			change.data[col.Name] = v
			pos += tupLen
		} else {
			v, notNull := maybeDereferenceTwice(ci.columnValues[pos])
			if notNull {
				change.data[col.Name] = v
			}
			pos++
		}
	}
	return ci.cdcStreamCols, change
}

func (ci *changeRowIterator) Close() error {
	return ci.iter.Close()
}

func maybeDereferenceTwice(i interface{}) (interface{}, bool) {
	v := reflect.Indirect(reflect.ValueOf(i))
	if v.Kind() != reflect.Ptr {
		return v.Interface(), true
	}
	if v.IsNil() {
		return nil, false
	}
	return reflect.Indirect(v).Interface(), true
}

func timeuuidToTimestamp(from gocql.UUID) int64 {
	return (from.Timestamp() - 0x01b21dd213814000) / 10
}
