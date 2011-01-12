package mongogo


import (
    "gobson"
    "sync"
    "os"
)


const (
    Strong = iota
    Monotonic
    Eventual
)

type Session struct {
    m sync.RWMutex
    consistency int
    cluster *mongoCluster
    socket *mongoSocket
    queryConfig query
    safe *queryOp
}

type Database struct {
    Session *Session
    Name string
}

type Collection struct {
    Session *Session
    Name string
}

type Query struct {
    m sync.Mutex
    session *Session
    query // Enables default settings in session.
}

type query struct {
    op queryOp
    prefetch float
}

type getLastError struct {
    CmdName int "getLastError"
    W int "w/c"
    WTimeout int "wtimeout/c"
    FSync bool "fsync/c"
}

type Iter struct {
    m sync.Mutex
    session *Session
    docData queue
    err os.Error
    gotReply cond
    op getMoreOp
    prefetch float
    pendingDocs int
    docsBeforeMore int
}

var NotFound = os.ErrorString("Document not found")

const defaultPrefetch = 0.25


// ---------------------------------------------------------------------------
// Public session methods.

// Return a database object, which allows further accessing any collections
// within it, or performing any database-level operations.  Creating this
// object is a very lightweight operation, and involves no network
// communication.
func (session *Session) DB(name string) Database {
    return Database{session, name}
}

// Return a collection object for the given database, which enables querying
// and modifying the content contained in the given collection. Creating this
// object is a very lightweight operation, and involves no network
// communication.
func (database Database) C(name string) Collection {
    return Collection{database.Session, database.Name + "." + name}
}

// Put back any reserved sockets in use and restart the consistency
// guarantees according to the existing consistency setting.
func (session *Session) Restart() {
    session.m.Lock()
    session.setSocket(nil)
    session.m.Unlock()
}

// Create a new session with the same parameters as the original session,
// including batch size, prefetching, safety mode, etc. Unlike Clone(),
// this will not share any sockets between the two sessions.
func (session *Session) New() *Session {
    session.m.Lock()
    clone := &Session{consistency: session.consistency,
                      cluster: session.cluster,
                      safe: session.safe,
                      queryConfig: session.queryConfig}
    session.m.Unlock()
    return clone
}

// Clone the session and return a new session with the same parameters as the
// original one, including batch size, prefetching, safety mode, etc. Unlike
// Unlike New(), in case a socket has already been reserved by the original
// session to preserve consistency requirements, the same socket will be shared
// with the new session before either session is garbage collected or
// Restart()ed.
func (session *Session) Clone() *Session {
    session.m.Lock()
    clone := &Session{consistency: session.consistency,
                      cluster: session.cluster,
                      safe: session.safe,
                      queryConfig: session.queryConfig}
    clone.setSocket(session.socket)
    session.m.Unlock()
    return clone
}

// Set the default batch size used when fetching documents from the database.
// It's possible to change this setting on a per-query basis as well, using
// the respective Batch() method.
//
// The default batch size is defined by the database itself.  As of this
// writing, MongoDB will use an initial size of min(100 docs, 4MB) on the
// first batch, and 4MB on remaining ones.
func (session *Session) Batch(size int) {
    session.m.Lock()
    session.queryConfig.op.limit = int32(size)
    session.m.Unlock()
}

// Set the default point at which the next batch of results will be requested.
// When there are p*batch_size remaining documents cached in an Iter, the next
// batch will be requested in background. For instance, when using this:
//
//     session.Batch(200)
//     session.Prefetch(0.25)
//
// and there are only 50 documents cached in the Iter to be processed, the
// next batch of 200 will be requested. It's possible to change this setting on
// a per-query basis as well, using the respective Prefetch() method.
//
// The default prefetch value is 0.25.
func (session *Session) Prefetch(p float) {
    session.m.Lock()
    session.queryConfig.prefetch = p
    session.m.Unlock()
}

// Put the session in unsafe mode. Writes will become fire-and-forget, without
// error checking.  The unsafe mode is faster since operations won't hold on
// waiting for a confirmation.  It's also unsafe, though! ;-)  In addition to
// disabling it entirely, the parameters of safety can also be tweaked via the
// Safe() method.  It's also possible to modify the safety settings on a
// per-query basis, using the respective Safe() and Unsafe() methods.
func (session *Session) Unsafe() {
    session.m.Lock()
    session.safe = nil
    session.m.Unlock()
}

// Put the session into safe mode.  Once in safe mode, This will 
func (session *Session) Safe(w, wtimeout int, fsync bool) {
    session.m.Lock()
    session.safe = &queryOp{query: &getLastError{1, w, wtimeout, fsync},
                            collection: "admin.$cmd", limit: -1}
    session.m.Unlock()
}

// Run the provided command and unmarshal its result in the respective
// argument. The cmd argument may be either a string with the command name
// itself, in which case an empty document of the form M{cmd: 1} will be
// used, or it may be a full command document.
func (session *Session) Run(cmd interface{}, result interface{}) os.Error {
    if name, ok := cmd.(string); ok {
        cmd = gobson.M{name: 1}
    }
    c := session.DB("admin").C("$cmd")
    return c.Find(cmd).One(result)
}

// Prepare a query using the provided document.  The document may be a map or
// a struct value capable of being marshalled with gobson.  The map may be a
// generic one using interface{}, such as gobson.M, or it may be a properly
// typed map. Further details of the query may be tweaked using the resulting
// Query value, and then executed using One() or Iter().
func (collection Collection) Find(query interface{}) *Query {
    session := collection.Session
    q := &Query{session:session, query:session.queryConfig}
    q.op.query = query
    q.op.collection = collection.Name
    return q
}

type LastError struct {
    Err string
    Code, N, Waited int
    WTimeout bool
    FSyncFiles int "fsyncFiles"
}

func (err *LastError) String() string {
    return err.Err
}


// Insert one or more documents in the respective collection.  In case
// the session is in safe mode (see Safe()) and an error happens while
// inserting the provided documents, the returned error will be of type
// (*mongogo.LastError).
func (collection Collection) Insert(docs ...interface{}) os.Error {
    socket, err := collection.Session.acquireSocket(true)
    if err != nil {
        return err
    }
    defer socket.Release()
    insert := &insertOp{collection.Name, docs}
    // XXX Lock session, or better: implement getSafe().
    if collection.Session.safe != nil {
        var mutex sync.Mutex
        var docData []byte
        mutex.Lock()
        query := *collection.Session.safe // Copy
        query.replyFunc = func(reply *replyOp, docNum int, _docData []byte) {
            docData = _docData
            mutex.Unlock()
        }
        err = socket.Query(insert, &query)
        if err != nil {
            return err
        }
        mutex.Lock() // Wait.
        result := &LastError{}
        gobson.Unmarshal(docData, &result)
        debugf("result: %#v", result)
        if result.Err != "" {
            err = result
        }
    } else {
        err = socket.Query(insert)
    }
    return err
}

// Set the batch size used when fetching documents from the database. It's
// possible to change this setting on a per-session basis as well, using
// the respective Batch() method.
//
// The default batch size is defined by the database itself.  As of this
// writing, MongoDB will use an initial size of min(100 docs, 4MB) on the
// first batch, and 4MB on remaining ones.
func (query *Query) Batch(size int) *Query {
    query.m.Lock()
    query.op.limit = int32(size)
    query.m.Unlock()
    return query
}

// Set the point at which the next batch of results will be requested. When
// there are p*batch_size remaining documents cached in an Iter, the next
// batch will be requested in background. For instance, when using this:
//
//     query.Batch(200).Prefetch(0.25)
//
// and there are only 50 documents cached in the Iter to be processed, the
// next batch of 200 will be requested. It's possible to change this setting on
// a per-session basis as well, using the respective Prefetch() method.
//
// The default prefetch value is 0.25.
func (query *Query) Prefetch(p float) *Query {
    query.m.Lock()
    query.prefetch = p
    query.m.Unlock()
    return query
}

// Skip the n initial documents from the query results.  Note that this only
// makes sense with capped collections where documents are naturally ordered
// by insertion time, or with sorted results.
func (query *Query) Skip(n int) *Query {
    query.m.Lock()
    query.op.skip = int32(n)
    query.m.Unlock()
    return query
}


type queryWrapper struct {
    Query interface{} "$query"
    OrderBy interface{} "$orderby/c"
}

func (query *Query) wrap() *queryWrapper {
    w, ok := query.op.query.(*queryWrapper)
    if !ok {
        w = &queryWrapper{Query: query.op.query}
        query.op.query = w
    }
    return w
}

// Sort documents according to the rules provided in the given document.
func (query *Query) Sort(order interface{}) *Query {
    // Could check other maps here too, but would need reflection and be
    // too expensive. Just prevent the most obvious problem instead.
    if m, ok := order.(gobson.M); ok && len(m) > 1 {
        panic("Can't sort using map of len > 1. Use gobson.D instead.")
    }
    query.m.Lock()
    w := query.wrap()
    w.OrderBy = order
    query.m.Unlock()
    return query
}

// Execute the query and unmarshal the first obtained document into the result
// argument.  The result must be a struct or map value capable of being
// unmarshalled into by gobson.
func (query *Query) One(result interface{}) (err os.Error) {
    query.m.Lock()
    session := query.session
    op := query.op // Copy.
    query.m.Unlock()

    socket, err := session.acquireSocket(false)
    if err != nil {
        return err
    }
    defer socket.Release()

    var mutex sync.Mutex
    var docData []byte

    mutex.Lock()

    op.limit = -1
    op.replyFunc = func(reply *replyOp, docNum int, _docData []byte) {
        docData = _docData
        mutex.Unlock()
    }

    err = socket.Query(&op)
    if err != nil {
        return err
    }

    // Unmarshal outside of the read goroutine to avoid blocking it.
    mutex.Lock()
    if docData == nil {
        return NotFound
    }

    err = gobson.Unmarshal(docData, result)
    if err == nil {
        debugf("Query %p document unmarshaled: %#v", query, result)
    } else {
        debugf("Query %p document unmarshaling failed: %#v", query, err)
    }
    return err
}

// Execute the query and return an iterator capable of going over all the
// results. This function will block until either a result is available or
// an error happened. Results will be returned in batches of configurable
// size (see Batch()) and more documents will be requested when a
// configurable threshold is reached (see Prefetch()).
func (query *Query) Iter() (iter *Iter, err os.Error) {
    query.m.Lock()
    session := query.session
    op := query.op
    prefetch := query.prefetch
    query.m.Unlock()

    socket, err := session.acquireSocket(false)
    if err != nil {
        return nil, err
    }
    defer socket.Release()

    iter = &Iter{session:session, prefetch:prefetch}
    iter.op.collection = op.collection
    iter.op.limit = op.limit

    op.replyFunc = iter.replyFunc()
    iter.op.replyFunc = op.replyFunc
    iter.pendingDocs++

    err = socket.Query(&op)
    if err != nil {
        return nil, err
    }

    return iter, nil
}

// Retrieve the next document from the result set, blocking if necessary.  If
// necessary, this method will also retrieve another batch of documents from
// the server, potentially in background (see Prefetch()).
func (iter *Iter) Next(result interface{}) (err os.Error) {
    iter.gotReply.Wait(func() bool {
        iter.m.Lock()
        if iter.err == nil && iter.pendingDocs > 0 && iter.docData.Len() == 0 {
            iter.m.Unlock()
            return false
        }
        return true
    })

    // Exhaust available data before returning any errors.
    if docData, ok := iter.docData.Pop().([]byte); ok {
        if iter.op.cursorId != 0 && iter.err == nil {
            iter.docsBeforeMore--
            if iter.docsBeforeMore == 0 {
                iter.getMore()
            }
        }
        iter.m.Unlock()
        err = gobson.Unmarshal(docData, result)
        if err == nil {
            debugf("Iter %p document unmarshaled: %#v", iter, result)
        } else {
            debugf("Iter %p document unmarshaling failed: %#v", iter, err)
        }
        return err
    } else if iter.err != nil {
        err := iter.err
        debugf("Iter %p returning error: %s", err)
        iter.m.Unlock()
        return err
    } else if iter.op.cursorId == 0 {
        debugf("Iter %p returning NotFound with cursor=0", iter)
        iter.m.Unlock()
        return NotFound
    }

    panic("Internal error: this should be unreachable")
    return
}



// ---------------------------------------------------------------------------
// Internal session handling helpers.

func newSession(consistency int, cluster *mongoCluster, socket *mongoSocket) (
        session *Session) {
    session = &Session{consistency: consistency, cluster: cluster}
    session.setSocket(socket)
    session.queryConfig.prefetch = defaultPrefetch
    session.Safe(0, 0, false)
    return session
}

func (session *Session) acquireSocket(write bool) (
        s *mongoSocket, err os.Error) {
    // XXX Must take into account consistency setting.
    session.m.RLock()
    s = session.socket
    // XXX Lock the server here?
    if s == nil || write && !s.server.Master {
        session.m.RUnlock()
        // Try again, with an exclusive lock now.
        session.m.Lock()
        s = session.socket
        if s == nil || write && !s.server.Master {
            s, err = session.cluster.AcquireSocket(write)
            if err == nil {
                session.setSocket(s)
            }
        }
        session.m.Unlock()
    } else {
        session.m.RUnlock()
        s.Acquire()
    }
    return s, nil
}

// Set the socket bound to this session.  With a bound socket, all operations
// with this session will use the given socket if possible. When not possible
// (e.g. attempting to write to a slave) acquireSocket() will replace the
// current socket.  Note that this method will properly refcount the socket up
// and down when setting/releasing.
func (session *Session) setSocket(socket *mongoSocket) {
    if session.socket != nil {
        session.socket.Release()
    }
    if socket != nil {
        socket.Acquire() // Hold a reference while the session is using it.
    }
    session.socket = socket
}

func (iter *Iter) replyFunc() replyFunc {
    return func(op *replyOp, docNum int, docData []byte) {
        iter.m.Lock()
        iter.pendingDocs--
        if docNum == -1 {
            iter.err = NotFound
            debugf("Iter %p has no documents.", iter)
        } else {
            rdocs := int(op.replyDocs)
            if docNum == 0 {
                iter.pendingDocs += rdocs-1
                iter.docsBeforeMore = rdocs-int(iter.prefetch*float(rdocs))
                iter.op.cursorId = op.cursorId
            }
            // XXX Handle errors and flags.
            debugf("Iter %p received reply document %d/%d", iter, docNum, rdocs)
            iter.docData.Push(docData)
        }
        iter.m.Unlock()
        iter.gotReply.Broadcast()
    }
}

func (iter *Iter) getMore() {
    socket, err := iter.session.acquireSocket(false)
    if err != nil {
        iter.err = err
        return
    }
    defer socket.Release()
    debugf("Iter %p requesting more documents", iter)
    iter.pendingDocs++
    err = socket.Query(&iter.op)
    if err != nil {
        iter.err = err
    }
}
