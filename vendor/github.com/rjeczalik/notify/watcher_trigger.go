// Copyright (c) 2014-2015 The Notify Authors. All rights reserved.
// Use of this source code is governed by the MIT license that can be
// found in the LICENSE file.

// +build darwin,kqueue dragonfly freebsd netbsd openbsd solaris

// watcher_trigger is used for FEN and kqueue which behave similarly:
// only files and dirs can be watched directly, but not files inside dirs.
// As a result Create events have to be generated by implementation when
// after Write event is returned for watched dir, it is rescanned and Create
// event is returned for new files and these are automatically added
// to watchlist. In case of removal of watched directory, native system returns
// events for all files, but for Rename, they also need to be generated.
// As a result native system works as somentrusting like trigger for rescan,
// but contains additional data about dir in which changes occurred. For files
// detailed data is returned.
// Usage of watcher_trigger requires:
// - trigger implementation,
// - encode func,
// - not2nat, nat2not maps.
// Required manual operations on filesystem can lead to loss of precision.

package notify

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// trigger is to be implemented by platform implementation like FEN or kqueue.
type trigger interface {
	// Close closes watcher's main native file descriptor.
	Close() error
	// Stop waiting for new events.
	Stop() error
	// Create new instance of watched.
	NewWatched(string, os.FileInfo) (*watched, error)
	// Record internally new *watched instance.
	Record(*watched)
	// Del removes internal copy of *watched instance.
	Del(*watched)
	// Watched returns *watched instance and native events for native type.
	Watched(interface{}) (*watched, int64, error)
	// Init initializes native watcher call.
	Init() error
	// Watch starts watching provided file/dir.
	Watch(os.FileInfo, *watched, int64) error
	// Unwatch stops watching provided file/dir.
	Unwatch(*watched) error
	// Wait for new events.
	Wait() (interface{}, error)
	// IsStop checks if Wait finished because of request watcher's stop.
	IsStop(n interface{}, err error) bool
}

// encode Event to native representation. Implementation is to be provided by
// platform specific implementation.
var encode func(Event, bool) int64

var (
	// nat2not matches native events to notify's ones. To be initialized by
	// platform dependent implementation.
	nat2not map[Event]Event
	// not2nat matches notify's events to native ones. To be initialized by
	// platform dependent implementation.
	not2nat map[Event]Event
)

// trg is a main structure implementing watcher.
type trg struct {
	sync.Mutex
	// s is a channel used to stop monitoring.
	s chan struct{}
	// c is a channel used to pass events further.
	c chan<- EventInfo
	// pthLkp is a data structure mapping file names with data about watching
	// represented by them files/directories.
	pthLkp map[string]*watched
	// t is a platform dependent implementation of trigger.
	t trigger
}

// newWatcher returns new watcher's implementation.
func newWatcher(c chan<- EventInfo) watcher {
	t := &trg{
		s:      make(chan struct{}, 1),
		pthLkp: make(map[string]*watched, 0),
		c:      c,
	}
	t.t = newTrigger(t.pthLkp)
	if err := t.t.Init(); err != nil {
		panic(err)
	}
	go t.monitor()
	return t
}

// Close implements watcher.
func (t *trg) Close() (err error) {
	t.Lock()
	if err = t.t.Stop(); err != nil {
		t.Unlock()
		return
	}
	<-t.s
	var e error
	for _, w := range t.pthLkp {
		if e = t.unwatch(w.p, w.fi); e != nil {
			dbgprintf("trg: unwatch %q failed: %q\n", w.p, e)
			err = nonil(err, e)
		}
	}
	if e = t.t.Close(); e != nil {
		dbgprintf("trg: closing native watch failed: %q\n", e)
		err = nonil(err, e)
	}
	t.Unlock()
	return
}

// send reported events one by one through chan.
func (t *trg) send(evn []event) {
	for i := range evn {
		t.c <- &evn[i]
	}
}

// singlewatch starts to watch given p file/directory.
func (t *trg) singlewatch(p string, e Event, direct mode, fi os.FileInfo) (err error) {
	w, ok := t.pthLkp[p]
	if !ok {
		if w, err = t.t.NewWatched(p, fi); err != nil {
			return
		}
	}
	switch direct {
	case dir:
		w.eDir |= e
	case ndir:
		w.eNonDir |= e
	case both:
		w.eDir |= e
		w.eNonDir |= e
	}
	if err = t.t.Watch(fi, w, encode(w.eDir|w.eNonDir, fi.IsDir())); err != nil {
		return
	}
	if !ok {
		t.t.Record(w)
		return nil
	}
	return errAlreadyWatched
}

// decode converts event received from native to notify.Event
// representation taking into account requested events (w).
func decode(o int64, w Event) (e Event) {
	for f, n := range nat2not {
		if o&int64(f) != 0 {
			if w&f != 0 {
				e |= f
			}
			if w&n != 0 {
				e |= n
			}
		}
	}

	return
}

func (t *trg) watch(p string, e Event, fi os.FileInfo) error {
	if err := t.singlewatch(p, e, dir, fi); err != nil {
		if err != errAlreadyWatched {
			return nil
		}
	}
	if fi.IsDir() {
		err := t.walk(p, func(fi os.FileInfo) (err error) {
			if err = t.singlewatch(filepath.Join(p, fi.Name()), e, ndir,
				fi); err != nil {
				if err != errAlreadyWatched {
					return
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// walk runs f func on each file/dir from p directory.
func (t *trg) walk(p string, fn func(os.FileInfo) error) error {
	fp, err := os.Open(p)
	if err != nil {
		return err
	}
	ls, err := fp.Readdir(0)
	fp.Close()
	if err != nil {
		return err
	}
	for i := range ls {
		if err := fn(ls[i]); err != nil {
			return err
		}
	}
	return nil
}

func (t *trg) unwatch(p string, fi os.FileInfo) error {
	if fi.IsDir() {
		err := t.walk(p, func(fi os.FileInfo) error {
			err := t.singleunwatch(filepath.Join(p, fi.Name()), ndir)
			if err != errNotWatched {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return t.singleunwatch(p, dir)
}

// Watch implements Watcher interface.
func (t *trg) Watch(p string, e Event) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	t.Lock()
	err = t.watch(p, e, fi)
	t.Unlock()
	return err
}

// Unwatch implements Watcher interface.
func (t *trg) Unwatch(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	t.Lock()
	err = t.unwatch(p, fi)
	t.Unlock()
	return err
}

// Rewatch implements Watcher interface.
//
// TODO(rjeczalik): This is a naive hack. Rewrite might help.
func (t *trg) Rewatch(p string, _, e Event) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	t.Lock()
	if err = t.unwatch(p, fi); err == nil {
		// TODO(rjeczalik): If watch fails then we leave trigger in inconsistent
		// state. Handle? Panic? Native version of rewatch?
		err = t.watch(p, e, fi)
	}
	t.Unlock()
	return nil
}

func (*trg) file(w *watched, n interface{}, e Event) (evn []event) {
	evn = append(evn, event{w.p, e, w.fi.IsDir(), n})
	return
}

func (t *trg) dir(w *watched, n interface{}, e, ge Event) (evn []event) {
	// If it's dir and delete we have to send it and continue, because
	// other processing relies on opening (in this case not existing) dir.
	// Events for contents of this dir are reported by native impl.
	// However events for rename must be generated for all monitored files
	// inside of moved directory, because native impl does not report it independently
	// for each file descriptor being moved in result of move action on
	// parent directory.
	if (ge & (not2nat[Rename] | not2nat[Remove])) != 0 {
		// Write is reported also for Remove on directory. Because of that
		// we have to filter it out explicitly.
		evn = append(evn, event{w.p, e & ^Write & ^not2nat[Write], true, n})
		if ge&not2nat[Rename] != 0 {
			for p := range t.pthLkp {
				if strings.HasPrefix(p, w.p+string(os.PathSeparator)) {
					if err := t.singleunwatch(p, both); err != nil && err != errNotWatched &&
						!os.IsNotExist(err) {
						dbgprintf("trg: failed stop watching moved file (%q): %q\n",
							p, err)
					}
					if (w.eDir|w.eNonDir)&(not2nat[Rename]|Rename) != 0 {
						evn = append(evn, event{
							p, (w.eDir | w.eNonDir) & e &^ Write &^ not2nat[Write],
							w.fi.IsDir(), nil,
						})
					}
				}
			}
		}
		t.t.Del(w)
		return
	}
	if (ge & not2nat[Write]) != 0 {
		switch err := t.walk(w.p, func(fi os.FileInfo) error {
			p := filepath.Join(w.p, fi.Name())
			switch err := t.singlewatch(p, w.eDir, ndir, fi); {
			case os.IsNotExist(err) && ((w.eDir & Remove) != 0):
				evn = append(evn, event{p, Remove, fi.IsDir(), n})
			case err == errAlreadyWatched:
			case err != nil:
				dbgprintf("trg: watching %q failed: %q", p, err)
			case (w.eDir & Create) != 0:
				evn = append(evn, event{p, Create, fi.IsDir(), n})
			default:
			}
			return nil
		}); {
		case os.IsNotExist(err):
			return
		case err != nil:
			dbgprintf("trg: dir processing failed: %q", err)
		default:
		}
	}
	return
}

type mode uint

const (
	dir mode = iota
	ndir
	both
)

// unwatch stops watching p file/directory.
func (t *trg) singleunwatch(p string, direct mode) error {
	w, ok := t.pthLkp[p]
	if !ok {
		return errNotWatched
	}
	switch direct {
	case dir:
		w.eDir = 0
	case ndir:
		w.eNonDir = 0
	case both:
		w.eDir, w.eNonDir = 0, 0
	}
	if err := t.t.Unwatch(w); err != nil {
		return err
	}
	if w.eNonDir|w.eDir != 0 {
		mod := dir
		if w.eNonDir == 0 {
			mod = ndir
		}
		if err := t.singlewatch(p, w.eNonDir|w.eDir, mod,
			w.fi); err != nil && err != errAlreadyWatched {
			return err
		}
	} else {
		t.t.Del(w)
	}
	return nil
}

func (t *trg) monitor() {
	var (
		n   interface{}
		err error
	)
	for {
		switch n, err = t.t.Wait(); {
		case err == syscall.EINTR:
		case t.t.IsStop(n, err):
			t.s <- struct{}{}
			return
		case err != nil:
			dbgprintf("trg: failed to read events: %q\n", err)
		default:
			t.send(t.process(n))
		}
	}
}

// process event returned by native call.
func (t *trg) process(n interface{}) (evn []event) {
	t.Lock()
	w, ge, err := t.t.Watched(n)
	if err != nil {
		t.Unlock()
		dbgprintf("trg: %v event lookup failed: %q", Event(ge), err)
		return
	}

	e := decode(ge, w.eDir|w.eNonDir)
	if ge&int64(not2nat[Remove]|not2nat[Rename]) == 0 {
		switch fi, err := os.Stat(w.p); {
		case err != nil:
		default:
			if err = t.t.Watch(fi, w, encode(w.eDir|w.eNonDir, fi.IsDir())); err != nil {
				dbgprintf("trg: %q is no longer watched: %q", w.p, err)
				t.t.Del(w)
			}
		}
	}
	if e == Event(0) && (!w.fi.IsDir() || (ge&int64(not2nat[Write])) == 0) {
		t.Unlock()
		return
	}

	if w.fi.IsDir() {
		evn = append(evn, t.dir(w, n, e, Event(ge))...)
	} else {
		evn = append(evn, t.file(w, n, e)...)
	}
	if Event(ge)&(not2nat[Remove]|not2nat[Rename]) != 0 {
		t.t.Del(w)
	}
	t.Unlock()
	return
}
