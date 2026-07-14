package main

import "go.etcd.io/bbolt"

var db *bbolt.DB

func openDB(path string) error {
    logDb("open %s", path)
    var opened, err = bbolt.Open(path, 0600, nil)
    if err != nil { return err }
    err = opened.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(watchesBucket)
        return err
    })
    if err != nil {
        opened.Close()
        return err
    }
    db = opened
    return nil
}

func closeDB() error {
    return db.Close()
}
