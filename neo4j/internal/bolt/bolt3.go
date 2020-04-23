/*
 * Copyright (c) 2002-2020 "Neo4j,"
 * Neo4j Sweden AB [http://neo4j.com]
 *
 * This file is part of Neo4j.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package bolt

import (
	"errors"
	"net"
	"time"

	"github.com/neo4j/neo4j-go-driver/neo4j/internal/db"
	"github.com/neo4j/neo4j-go-driver/neo4j/internal/packstream"
)

const (
	msgV3Reset      packstream.StructTag = 0x0f
	msgV3Run        packstream.StructTag = 0x10
	msgV3DiscardAll packstream.StructTag = 0x2f
	msgV3PullAll    packstream.StructTag = 0x3f
	msgV3Record     packstream.StructTag = 0x71
	msgV3Success    packstream.StructTag = 0x70
	msgV3Ignored    packstream.StructTag = 0x7e
	msgV3Failure    packstream.StructTag = 0x7f
	msgV3Hello      packstream.StructTag = 0x01
	msgV3Goodbye    packstream.StructTag = 0x02
	msgV3Begin      packstream.StructTag = 0x11
	msgV3Commit     packstream.StructTag = 0x12
	msgV3Rollback   packstream.StructTag = 0x13
)

const userAgent = "Go Driver/1.8"

const (
	bolt3_ready       = iota // Ready for use
	bolt3_streaming          // Receiving result from auto commit query
	bolt3_pendingtx          // Transaction has been requested but not applied
	bolt3_tx                 // Transaction pending
	bolt3_streamingtx        // Receiving result from a query within a transaction
	bolt3_failed             // Recoverable error, needs reset
	bolt3_dead               // Non recoverable protocol or connection error
)

type internalTx struct {
	mode      db.AccessMode
	bookmarks []string
	timeout   time.Duration
	txMeta    map[string]interface{}
}

func (i *internalTx) toMeta() map[string]interface{} {
	mode := "w"
	if i.mode == db.ReadMode {
		mode = "r"
	}
	meta := map[string]interface{}{
		"mode": mode,
	}
	if len(i.bookmarks) > 0 {
		meta["bookmarks"] = i.bookmarks
	}
	ts := int(i.timeout.Seconds())
	if ts > 0 {
		meta["tx_timeout"] = ts
	}
	if len(i.txMeta) > 0 {
		meta["tx_metadata"] = i.txMeta
	}
	return meta
}

type bolt3 struct {
	state         int
	txId          int64
	streamId      int64
	streamKeys    []string
	conn          net.Conn
	serverName    string
	chunker       *chunker
	dechunker     *dechunker
	packer        *packstream.Packer
	unpacker      *packstream.Unpacker
	connId        string
	serverVersion string
	tfirst        int64       // Time that server started streaming
	pendingTx     *internalTx // Stashed away when tx started explcitly
	bookmark      string      // Last bookmark
	birthDate     time.Time
}

func NewBolt3(serverName string, conn net.Conn) *bolt3 {
	chunker := newChunker(conn)
	dechunker := newDechunker(conn)

	return &bolt3{
		state:      bolt3_dead,
		conn:       conn,
		serverName: serverName,
		chunker:    chunker,
		dechunker:  dechunker,
		packer:     packstream.NewPacker(chunker, dehydrate),
		unpacker:   packstream.NewUnpacker(dechunker),
		birthDate:  time.Now(),
	}
}

func (b *bolt3) ServerName() string {
	return b.serverName
}

func (b *bolt3) ServerVersion() string {
	return b.serverVersion
}

func (b *bolt3) appendMsg(tag packstream.StructTag, field ...interface{}) error {
	b.chunker.beginMessage()
	// Setup the message and let packstream write the packed bytes to the chunk
	if err := b.packer.PackStruct(tag, field...); err != nil {
		// At this point we do not know the state of what has been written to the chunks.
		// Either we should support rolling back whatever that has been written or just
		// bail out this session.
		b.state = bolt3_dead
		return err
	}
	b.chunker.endMessage()
	return nil
}

func (b *bolt3) sendMsg(tag packstream.StructTag, field ...interface{}) error {
	if err := b.appendMsg(tag, field...); err != nil {
		return err
	}
	if err := b.chunker.send(); err != nil {
		b.state = bolt3_dead
		return err
	}
	return nil
}

func (b *bolt3) receiveMsg() (interface{}, error) {
	if err := b.dechunker.beginMessage(); err != nil {
		b.state = bolt3_dead
		return nil, err
	}

	msg, err := b.unpacker.UnpackStruct(hydrate)
	if err != nil {
		b.state = bolt3_dead
		return nil, err
	}

	if err = b.dechunker.endMessage(); err != nil {
		b.state = bolt3_dead
		return nil, err
	}

	return msg, nil
}

// Receives a message that is assumed to be a success response or a failure in response
// to a sent command.
func (b *bolt3) receiveSuccess() (*successResponse, error) {
	msg, err := b.receiveMsg()
	if err != nil {
		return nil, err
	}

	switch v := msg.(type) {
	case *successResponse:
		return v, nil
	case *db.DatabaseError:
		b.state = bolt3_failed
		return nil, v
	}
	b.state = bolt3_dead
	return nil, errors.New("Unknown response")
}

func (b *bolt3) connect(auth map[string]interface{}) error {
	// Only allowed to connect when in disconnected state
	if err := assertState(b.state, bolt3_dead); err != nil {
		return err
	}

	hello := map[string]interface{}{
		"user_agent": userAgent,
	}
	// Merge authentication info into hello message
	for k, v := range auth {
		_, exists := hello[k]
		if exists {
			continue
		}
		hello[k] = v
	}

	// Send hello message
	if err := b.sendMsg(msgV3Hello, hello); err != nil {
		return err
	}

	succRes, err := b.receiveSuccess()
	if err != nil {
		return err
	}
	helloRes := succRes.hello()
	if helloRes == nil {
		return errors.New("proto error")
	}
	b.connId = helloRes.connectionId
	b.serverVersion = helloRes.server

	// Transition into ready state
	b.state = bolt3_ready
	return nil
}

func (b *bolt3) TxBegin(
	mode db.AccessMode, bookmarks []string, timeout time.Duration, txMeta map[string]interface{}) (db.Handle, error) {

	// Ok, to begin transaction while streaming auto-commit, just empty the stream and continue.
	if b.state == bolt3_streaming {
		if err := b.consumeStream(); err != nil {
			return nil, err
		}
	}

	if err := assertState(b.state, bolt3_ready); err != nil {
		return nil, err
	}

	// Stash this into pending internal tx
	b.pendingTx = &internalTx{
		mode:      mode,
		bookmarks: bookmarks,
		timeout:   timeout,
		txMeta:    txMeta,
	}

	b.txId = time.Now().Unix()
	b.state = bolt3_pendingtx

	return b.txId, nil
}

func (b *bolt3) TxCommit(txh db.Handle) error {
	if err := assertHandle(b.txId, txh); err != nil {
		return err
	}

	// Nothing to do, a transaction started but no commands were issued on it, server is unaware
	if b.state == bolt3_pendingtx {
		b.state = bolt3_ready
		return nil
	}

	// Consume pending stream if any to turn state from streamingtx to tx
	if b.state == bolt3_streamingtx {
		if err := b.consumeStream(); err != nil {
			return err
		}
	}

	// Should be in vanilla tx state now
	if err := assertState(b.state, bolt3_tx); err != nil {
		return err
	}

	// Send request to server to commit
	if err := b.sendMsg(msgV3Commit); err != nil {
		return err
	}

	// Evaluate server response
	succRes, err := b.receiveSuccess()
	if err != nil {
		return err
	}
	commitSuccess := succRes.commit()
	if commitSuccess == nil {
		b.state = bolt3_dead
		return errors.New("Parser error")
	}

	// Keep track of bookmark
	if len(commitSuccess.bookmark) > 0 {
		b.bookmark = commitSuccess.bookmark
	}

	// Transition into ready state
	b.state = bolt3_ready
	return nil
}

func (b *bolt3) TxRollback(txh db.Handle) error {
	if err := assertHandle(b.txId, txh); err != nil {
		return err
	}

	// Nothing to do, a transaction started but no commands were issued on it
	if b.state == bolt3_pendingtx {
		b.state = bolt3_ready
		return nil
	}

	// Can not send rollback while still streaming, consume to turn state into tx
	if b.state == bolt3_streamingtx {
		if err := b.consumeStream(); err != nil {
			return err
		}
	}

	// Should be in vanilla tx state now
	if err := assertState(b.state, bolt3_tx); err != nil {
		return err
	}

	// Send rollback request to server
	if err := b.sendMsg(msgV3Rollback); err != nil {
		return err
	}

	if _, err := b.receiveSuccess(); err != nil {
		return err
	}

	// Transition into ready state
	b.state = bolt3_ready
	return nil
}

// Discards all records, keeps bookmark
func (b *bolt3) consumeStream() error {
	// Anything to do?
	if b.state != bolt3_streaming && b.state != bolt3_streamingtx {
		return nil
	}

	for {
		_, sum, err := b.Next(b.streamId)
		if err != nil {
			return err
		}
		if sum != nil {
			break
		}
	}
	return nil
}

func (b *bolt3) run(cypher string, params map[string]interface{}, tx *internalTx) (*db.Stream, error) {
	// If already streaming, consume the whole thing first
	if err := b.consumeStream(); err != nil {
		return nil, err
	}

	if err := assertStates(b.state, []int{bolt3_tx, bolt3_ready, bolt3_pendingtx}); err != nil {
		return nil, err
	}

	var meta map[string]interface{}
	if tx != nil {
		meta = tx.toMeta()
	}

	// Append lazy begin transaction message
	if b.state == bolt3_pendingtx {
		if err := b.appendMsg(msgV3Begin, meta); err != nil {
			return nil, err
		}
		meta = nil
	}

	// Append run message
	if err := b.appendMsg(msgV3Run, cypher, params, meta); err != nil {
		return nil, err
	}

	// Append pull all message and send it all
	if err := b.sendMsg(msgV3PullAll); err != nil {
		return nil, err
	}

	// Process server responses
	// Receive confirmation of transaction begin if it was started above
	if b.state == bolt3_pendingtx {
		if _, err := b.receiveSuccess(); err != nil {
			return nil, err
		}
		b.state = bolt3_tx
	}

	// Receive confirmation of run message
	res, err := b.receiveSuccess()
	if err != nil {
		return nil, err
	}
	// Extract the RUN response from success response
	runRes := res.run()
	if runRes == nil {
		b.state = bolt3_dead
		return nil, errors.New("parse fail, proto error")
	}
	b.tfirst = runRes.t_first
	b.streamKeys = runRes.fields
	// Change state to streaming
	if b.state == bolt3_ready {
		b.state = bolt3_streaming
	} else {
		b.state = bolt3_streamingtx
	}

	b.streamId = time.Now().Unix()
	stream := &db.Stream{Keys: b.streamKeys, Handle: b.streamId}
	return stream, nil
}

func (b *bolt3) Run(
	cypher string, params map[string]interface{}, mode db.AccessMode,
	bookmarks []string, timeout time.Duration, txMeta map[string]interface{}) (*db.Stream, error) {

	if err := assertStates(b.state, []int{bolt3_streaming, bolt3_ready}); err != nil {
		return nil, err
	}

	tx := internalTx{
		mode:      mode,
		bookmarks: bookmarks,
		timeout:   timeout,
		txMeta:    txMeta,
	}
	return b.run(cypher, params, &tx)
}

func (b *bolt3) RunTx(txh db.Handle, cypher string, params map[string]interface{}) (*db.Stream, error) {
	if err := assertHandle(b.txId, txh); err != nil {
		return nil, err
	}

	stream, err := b.run(cypher, params, b.pendingTx)
	b.pendingTx = nil
	return stream, err
}

// Reads one record from the stream.
func (b *bolt3) Next(shandle db.Handle) (*db.Record, *db.Summary, error) {
	if err := assertHandle(b.streamId, shandle); err != nil {
		return nil, nil, err
	}

	if err := assertStates(b.state, []int{bolt3_streaming, bolt3_streamingtx}); err != nil {
		return nil, nil, err
	}

	res, err := b.receiveMsg()
	if err != nil {
		return nil, nil, err
	}

	switch x := res.(type) {
	case *recordResponse:
		rec := &db.Record{Keys: b.streamKeys, Values: x.values}
		return rec, nil, nil
	case *successResponse:
		// End of stream
		// Parse summary
		sum := x.summary()
		if sum == nil {
			b.state = bolt3_dead
			return nil, nil, errors.New("Failed to parse summary")
		}
		if b.state == bolt3_streamingtx {
			b.state = bolt3_tx
		} else {
			b.state = bolt3_ready
			// Keep bookmark for auto-commit tx
			if len(sum.Bookmark) > 0 {
				b.bookmark = sum.Bookmark
			}
		}
		b.streamId = 0
		// Add some extras to the summary
		sum.ServerVersion = b.serverVersion
		sum.ServerName = b.serverName
		sum.TFirst = b.tfirst
		return nil, sum, nil
	case *db.DatabaseError:
		b.state = bolt3_failed
		return nil, nil, x
	default:
		b.state = bolt3_dead
		return nil, nil, errors.New("Unknown response")
	}
}

func (b *bolt3) Bookmark() string {
	return b.bookmark
}

func (b *bolt3) IsAlive() bool {
	return b.state != bolt3_dead
}

func (b *bolt3) Birthdate() time.Time {
	return b.birthDate
}

func (b *bolt3) Reset() {
	defer func() {
		// Reset internal state
		b.txId = 0
		b.streamId = 0
		b.streamKeys = []string{}
		b.bookmark = ""
		b.pendingTx = nil
	}()

	if b.state == bolt3_ready || b.state == bolt3_dead {
		// No need for reset
		return
	}

	// Send the reset message to the server
	err := b.sendMsg(msgV3Reset)
	if err != nil {
		return
	}

	// If the server was streaming we need to clean everything that
	// might have been sent by the server before it received the reset.
	drained := false
	for !drained {
		res, err := b.receiveMsg()
		if err != nil {
			return
		}
		switch x := res.(type) {
		case *recordResponse, *ignoredResponse:
			// Just throw away. We should only get record responses while in streaming mode.
		case *db.DatabaseError:
			// This could mean that the reset command failed for some reason, could also
			// mean some other command that failed but as long as we never have unconfirmed
			// commands out of the handling functions this should mean that the reset failed.
			b.state = bolt3_dead
			return
		case *successResponse:
			// This could indicate either end of a stream have been sent right before
			// the reset or it could be a confirmation of the reset.
			sum := x.summary()
			drained = sum == nil
		}
	}

	b.state = bolt3_ready
}

func (b *bolt3) GetRoutingTable(context map[string]string) (*db.RoutingTable, error) {
	if err := assertState(b.state, bolt3_ready); err != nil {
		return nil, err
	}
	return getRoutingTable(b, context)
}

// Beware, could be called on another thread when driver is closed.
func (b *bolt3) Close() {
	if b.state != bolt3_dead {
		b.sendMsg(msgV3Goodbye)
	}
	b.conn.Close()
	b.state = bolt3_dead
}
