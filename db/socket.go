// Copyright © 2016 Abcum Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package db

import (
	"fmt"
	"sync"

	"context"

	"github.com/abcum/fibre"

	"github.com/abcum/surreal/cnf"
	"github.com/abcum/surreal/sql"
	"github.com/abcum/surreal/util/data"
	"github.com/abcum/surreal/util/keys"
	"github.com/abcum/surreal/util/uuid"
)

type socket struct {
	ns    string
	db    string
	mutex sync.Mutex
	fibre *fibre.Context
	items map[string][]interface{}
	lives map[string]*sql.LiveStatement
}

func clear(id string) {
	go func() {
		sockets.Range(func(key, val interface{}) bool {
			val.(*socket).clear(id)
			return true
		})
	}()
}

func flush(id string) {
	go func() {
		sockets.Range(func(key, val interface{}) bool {
			val.(*socket).flush(id)
			return true
		})
	}()
}

func (s *socket) ctx() (ctx context.Context) {

	ctx = context.Background()

	auth := s.fibre.Get(ctxKeyAuth).(*cnf.Auth)

	vars := data.New()
	vars.Set(ENV, varKeyEnv)
	vars.Set(auth.Data, varKeyAuth)
	vars.Set(auth.Scope, varKeyScope)
	vars.Set(session(s.fibre), varKeySession)
	ctx = context.WithValue(ctx, ctxKeyVars, vars)
	ctx = context.WithValue(ctx, ctxKeyKind, auth.Kind)

	return

}

func (s *socket) queue(id, query, action string, result interface{}) {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.items[id] = append(s.items[id], &Dispatch{
		Query:  query,
		Action: action,
		Result: result,
	})

}

func (s *socket) clear(id string) (err error) {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.items, id)

	return

}

func (s *socket) flush(id string) (err error) {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// If there are no pending message
	// notifications for this socket
	// then ignore this method call.

	if len(s.items[id]) == 0 {
		return nil
	}

	// Create a new rpc notification
	// object so that we can send the
	// batch changes in one go.

	obj := &fibre.RPCNotification{
		Method: "notify",
		Params: s.items[id],
	}

	// Notify the websocket connection
	// y sending an RPCNotification type
	// to the notify channel.

	s.fibre.Socket().Notify(obj)

	// Make sure that we clear all the
	// pending message notifications
	// for this socket when done.

	delete(s.items, id)

	return

}

func (s *socket) check(e *executor, ctx context.Context, ns, db, tb string) (err error) {

	var tbv *sql.DefineTableStatement

	// If we are authenticated using DB, NS,
	// or KV permissions level, then we can
	// ignore all permissions checks.

	if perm(ctx) < cnf.AuthSC {
		return nil
	}

	// First check that the NS exists, as
	// otherwise, the scoped authentication
	// request can not do anything.

	_, err = e.dbo.GetNS(ctx, ns)
	if err != nil {
		return err
	}

	// Next check that the DB exists, as
	// otherwise, the scoped authentication
	// request can not do anything.

	_, err = e.dbo.GetDB(ctx, ns, db)
	if err != nil {
		return err
	}

	// Then check that the TB exists, as
	// otherwise, the scoped authentication
	// request can not do anything.

	tbv, err = e.dbo.GetTB(ctx, ns, db, tb)
	if err != nil {
		return err
	}

	// If the table has any permissions
	// specified, then let's check if this
	// query is allowed access to the table.

	switch p := tbv.Perms.(type) {
	case *sql.PermExpression:
		return e.fetchPerms(ctx, p.Select, tbv.Name)
	default:
		return &PermsError{table: tb}
	}

}

func (s *socket) deregister(id string) {

	sockets.Delete(id)

	ctx := context.Background()

	txn, _ := db.Begin(ctx, true)

	defer txn.Commit()

	for id, stm := range s.lives {

		for _, w := range stm.What {

			switch what := w.(type) {

			case *sql.Table:

				key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.TB, LV: id}
				txn.Clr(ctx, key.Encode())

			case *sql.Ident:

				key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.VA, LV: id}
				txn.Clr(ctx, key.Encode())

			}

		}

	}

}

func (s *socket) executeLive(e *executor, ctx context.Context, stm *sql.LiveStatement) (out []interface{}, err error) {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Generate a new query uuid.

	stm.ID = uuid.New().String()

	// Store the live query on the socket.

	s.lives[stm.ID] = stm

	// Return the query id to the user.

	out = append(out, stm.ID)

	// Store the live query in the database layer.

	for key, val := range stm.What {
		w, err := e.fetch(ctx, val, nil)
		if err != nil {
			return nil, err
		}
		stm.What[key] = w
	}

	for _, w := range stm.What {

		switch what := w.(type) {

		default:
			return nil, fmt.Errorf("Can not execute LIVE query using value '%v'", what)

		case *sql.Table:

			key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.TB, LV: stm.ID}
			if _, err = e.dbo.Put(ctx, 0, key.Encode(), stm.Encode()); err != nil {
				return nil, err
			}

		case *sql.Ident:

			key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.VA, LV: stm.ID}
			if _, err = e.dbo.Put(ctx, 0, key.Encode(), stm.Encode()); err != nil {
				return nil, err
			}

		}

	}

	return

}

func (s *socket) executeKill(e *executor, ctx context.Context, stm *sql.KillStatement) (out []interface{}, err error) {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Remove the live query from the database layer.

	for key, val := range stm.What {
		w, err := e.fetch(ctx, val, nil)
		if err != nil {
			return nil, err
		}
		stm.What[key] = w
	}

	for _, w := range stm.What {

		switch what := w.(type) {

		default:
			return nil, fmt.Errorf("Can not execute KILL query using value '%v'", what)

		case string:

			if qry, ok := s.lives[what]; ok {

				// Delete the live query from the saved queries.

				delete(s.lives, qry.ID)

				// Delete the live query from the database layer.

				for _, w := range qry.What {

					switch what := w.(type) {

					case *sql.Table:
						key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.TB, LV: qry.ID}
						_, err = e.dbo.Clr(ctx, key.Encode())

					case *sql.Ident:
						key := &keys.LV{KV: KV, NS: s.ns, DB: s.db, TB: what.VA, LV: qry.ID}
						_, err = e.dbo.Clr(ctx, key.Encode())

					}

				}

			}

		}

	}

	return

}
