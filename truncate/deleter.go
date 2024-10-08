//
// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package truncate

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/spanner"
)

// Status is a delete status.
type status int

const (
	statusAnalyzing       status = iota // Status for calculating the total rows in the table.
	statusWaiting                       // Status for waiting for dependent tables being deleted.
	statusDeleting                      // Status for deleting rows.
	statusCascadeDeleting               // Status for deleting rows by parent in cascaded way.
	statusCompleted                     // Status for delete completed.
)

const thirtySeconds = 30 * time.Second

// deleter deletes all rows from the table.
type deleter struct {
	tableName   string
	whereClause string
	client      *spanner.Client
	status      status

	// Total rows in the table.
	// Once set, we don't update this number even if new rows are added to the table.
	totalRows uint64

	// Remained rows in the table.
	remainedRows uint64
}

// deleteRows deletes rows from the table using PDML.
func (d *deleter) deleteRows(ctx context.Context) error {
	d.status = statusDeleting
	rawStatement := fmt.Sprintf("DELETE FROM `%s` WHERE %s", d.tableName, d.whereClause)
	stmt := spanner.NewStatement(rawStatement)
	_, err := d.client.PartitionedUpdate(ctx, stmt)
	return err
}

// When parent deletion started, change child status unless the child deletion has already completed.
func (d *deleter) parentDeletionStarted() {
	if d.status != statusCompleted {
		d.status = statusCascadeDeleting
	}
}

// startRowCountUpdater starts periodical row count in another goroutine.
func (d *deleter) startRowCountUpdater(ctx context.Context) {
	go func() {
		for {
			if d.status == statusCompleted {
				return
			}

			begin := time.Now()
			// Ignore error as it could be a temporal error.
			d.updateRowCount(ctx)
			sleepDuration := time.Since(begin) * 10
			// Sleep for a while to minimize the impact on CPU usage caused by SELECT COUNT(*) queries.
			if sleepDuration > thirtySeconds {
				sleepDuration = thirtySeconds
			}
			time.Sleep(sleepDuration)
		}
	}()
}

func (d *deleter) updateRowCount(ctx context.Context) error {
	stmt := spanner.NewStatement(fmt.Sprintf("SELECT COUNT(*) as count FROM `%s` WHERE %s", d.tableName, d.whereClause))
	var count int64

	// Use stale read to minimize the impact on the leader replica.
	txn := d.client.Single().WithTimestampBound(spanner.ExactStaleness(time.Second))
	if err := txn.Query(ctx, stmt).Do(func(r *spanner.Row) error {
		return r.ColumnByName("count", &count)
	}); err != nil {
		return err
	}

	if d.totalRows == 0 {
		d.totalRows = uint64(count)
	}
	d.remainedRows = uint64(count)

	if count == 0 {
		d.status = statusCompleted
	} else if d.status == statusAnalyzing {
		d.status = statusWaiting
	}

	return nil
}
