// Copyright 2019 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package importccl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/blobs"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/distsql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/stretchr/testify/assert"
)

type testSpec struct {
	inputFormat roachpb.IOFileFormat_FileFormat
	inputs      map[int32]string
	tables      map[string]*execinfrapb.ReadImportDataSpec_ImportTable
}

// Given test spec returns ReadImportDataSpec suitable creating input converter.
func (spec *testSpec) getConverterSpec() *execinfrapb.ReadImportDataSpec {
	pgDumpOptions := roachpb.PgDumpOptions{MaxRowSize: 64 * 1024}
	pgCopyOptions := roachpb.PgCopyOptions{
		Delimiter:  '\t',
		Null:       `\N`,
		MaxRowSize: 4096,
	}
	mysqlOutOptions := roachpb.MySQLOutfileOptions{
		FieldSeparator: ',',
		RowSeparator:   '\n',
		HasEscape:      true,
		Escape:         '\\',
		Enclose:        roachpb.MySQLOutfileOptions_Always,
		Encloser:       '"',
	}

	return &execinfrapb.ReadImportDataSpec{
		Format: roachpb.IOFileFormat{
			Format:   spec.inputFormat,
			PgDump:   pgDumpOptions,
			PgCopy:   pgCopyOptions,
			MysqlOut: mysqlOutOptions,
		},
		Tables:            spec.tables,
		Uri:               spec.inputs,
		ReaderParallelism: 1, // Make tests deterministic
	}
}

func TestConverterFlushesBatches(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Reset batch size setting upon test completion.
	defer row.TestingSetDatumRowConverterBatchSize(0)()

	// Helper to generate test name.
	testName := func(inputFormat roachpb.IOFileFormat_FileFormat, batchSize int) string {
		switch batchSize {
		case 0:
			return fmt.Sprintf("%s-default-batch-size", inputFormat)
		case 1:
			return fmt.Sprintf("%s-always-flush", inputFormat)
		default:
			return fmt.Sprintf("%s-flush-%d-records", inputFormat, batchSize)
		}
	}

	ctx := context.Background()
	evalCtx := tree.MakeTestingEvalContext(nil)

	tests := []testSpec{
		newTestSpec(t, roachpb.IOFileFormat_CSV, "testdata/csv/data-0"),
		newTestSpec(t, roachpb.IOFileFormat_Mysqldump, "testdata/mysqldump/simple.sql"),
		newTestSpec(t, roachpb.IOFileFormat_PgDump, "testdata/pgdump/simple.sql"),
	}

	const endBatchSize = -1

	for _, testCase := range tests {
		expectedNumRecords := 0
		expectedNumBatches := 0
		converterSpec := testCase.getConverterSpec()

		// Run multiple tests, increasing batch size until it exceeds the
		// total number of records. When batch size is 0, we run converters
		// with the default batch size, and use that run to figure out the
		// expected number of records and batches for the subsequent run.
		for batchSize := 0; batchSize != endBatchSize; {
			t.Run(testName(testCase.inputFormat, batchSize), func(t *testing.T) {
				if batchSize > 0 {
					row.TestingSetDatumRowConverterBatchSize(batchSize)
				}

				kvCh := make(chan row.KVBatch, batchSize)
				conv, err := makeInputConverter(converterSpec, &evalCtx, kvCh)
				if err != nil {
					t.Fatalf("makeInputConverter() error = %v", err)
				}

				group := ctxgroup.WithContext(ctx)
				group.Go(func() error {
					defer close(kvCh)
					return conv.readFiles(ctx, testCase.inputs, nil, converterSpec.Format, externalStorageFactory)
				})

				lastBatch := 0
				testNumRecords := 0
				testNumBatches := 0

				// Read from the channel; we expect batches of testCase.batchSize
				// size, with the exception of the last batch.
				for batch := range kvCh {
					if batchSize > 0 {
						assert.True(t, lastBatch == 0 || lastBatch == batchSize)
					}
					lastBatch = len(batch.KVs)
					testNumRecords += lastBatch
					testNumBatches++
				}
				if err := group.Wait(); err != nil {
					t.Fatalf("Conversion failed: %v", err)
				}

				if batchSize == 0 {
					expectedNumRecords = testNumRecords
					// Next batch: flush every record.
					batchSize = 1
					expectedNumBatches = expectedNumRecords
				} else if batchSize > expectedNumRecords {
					// Done with this test case.
					batchSize = endBatchSize
					return
				} else {
					// Number of records and batches ought to be correct.
					assert.Equal(t, expectedNumRecords, testNumRecords)
					assert.Equal(t, expectedNumBatches, testNumBatches)

					// Progressively increase the batch size.
					batchSize += (batchSize << 2)
					expectedNumBatches = int(math.Ceil(float64(expectedNumRecords) / float64(batchSize)))
				}
			})
		}
	}
}

// A RowReceiver implementation which fails the test if it receives an error.
type errorReportingRowReceiver struct {
	t *testing.T
}

var _ execinfra.RowReceiver = &errorReportingRowReceiver{}

func (r *errorReportingRowReceiver) Push(
	row sqlbase.EncDatumRow, meta *execinfrapb.ProducerMetadata,
) execinfra.ConsumerStatus {
	if r.t.Failed() || (meta != nil && meta.Err != nil) {
		if !r.t.Failed() {
			r.t.Fail()
		}
		r.t.Logf("receiver got an error: %v", meta.Err)
		return execinfra.ConsumerClosed
	}
	return execinfra.NeedMoreRows
}

func (r *errorReportingRowReceiver) ProducerDone() {}
func (r *errorReportingRowReceiver) Types() []types.T {
	return nil
}

// A bulk adder implementation that verifies
// that all of the keys added are greater or equal to the min key
type ensureMinKeyAdder struct {
	minKey  *roachpb.Key
	onFlush func()
}

var _ storagebase.BulkAdder = &ensureMinKeyAdder{}

func (a *ensureMinKeyAdder) Add(_ context.Context, k roachpb.Key, _ []byte) error {
	if a.minKey != nil && a.minKey.Compare(k) > 0 {
		return fmt.Errorf("%v > %v", a.minKey, k)
	}
	return nil
}
func (a *ensureMinKeyAdder) Flush(_ context.Context) error {
	if a.onFlush != nil {
		a.onFlush()
	}
	return nil
}

func (*ensureMinKeyAdder) IsEmpty() bool                     { return true }
func (*ensureMinKeyAdder) CurrentBufferFill() float32        { return 0 }
func (*ensureMinKeyAdder) GetSummary() roachpb.BulkOpSummary { return roachpb.BulkOpSummary{} }
func (*ensureMinKeyAdder) Close(_ context.Context)           {}
func (a *ensureMinKeyAdder) SetOnFlush(f func())             { a.onFlush = f }

var eofOffset int64 = math.MaxInt64

func TestImportIgnoresProcessedFiles(t *testing.T) {
	defer leaktest.AfterTest(t)()

	evalCtx := tree.MakeTestingEvalContext(nil)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings:        &cluster.Settings{},
			ExternalStorage: externalStorageFactory,
			BulkAdder: func(
				_ context.Context, _ *client.DB, _ hlc.Timestamp,
				_ storagebase.BulkAdderOptions) (storagebase.BulkAdder, error) {
				return &ensureMinKeyAdder{}, nil
			},
		},
	}

	// In this test, we'll specify import files that do not exist, but mark
	// those files fully processed. The converters should not attempt to even
	// open these files (and if they do, we should report a test failure)
	tests := []struct {
		name         string
		spec         testSpec
		inputOffsets []int64 // List of file ids that were fully processed
	}{
		{
			"csv-two-invalid",
			newTestSpec(t, roachpb.IOFileFormat_CSV, "__invalid__", "testdata/csv/data-0", "/_/missing/_"),
			[]int64{eofOffset, 0, eofOffset},
		},
		{
			"csv-all-invalid",
			newTestSpec(t, roachpb.IOFileFormat_CSV, "__invalid__", "../../&"),
			[]int64{eofOffset, eofOffset},
		},
		{
			"csv-all-valid",
			newTestSpec(t, roachpb.IOFileFormat_CSV, "testdata/csv/data-0"),
			[]int64{0},
		},
		{
			"mysql-one-invalid",
			newTestSpec(t, roachpb.IOFileFormat_Mysqldump, "testdata/mysqldump/simple.sql", "/_/missing/_"),
			[]int64{0, eofOffset},
		},
		{
			"pgdump-one-input",
			newTestSpec(t, roachpb.IOFileFormat_PgDump, "testdata/pgdump/simple.sql"),
			[]int64{0},
		},
	}

	// Configures import spec to have appropriate input offsets set.
	setInputOffsets := func(
		t *testing.T, spec *execinfrapb.ReadImportDataSpec, offsets []int64,
	) *execinfrapb.ReadImportDataSpec {
		if len(spec.Uri) != len(offsets) {
			t.Fatal("Expected matching number of input offsets")
		}
		spec.ResumePos = make(map[int32]int64)
		for id, offset := range offsets {
			if offset > 0 {
				spec.ResumePos[int32(id)] = offset
			}
		}
		return spec
	}

	for _, testCase := range tests {
		t.Run(fmt.Sprintf("processes-files-once-%s", testCase.name), func(t *testing.T) {
			spec := setInputOffsets(t, testCase.spec.getConverterSpec(), testCase.inputOffsets)

			processor, err := newReadImportDataProcessor(flowCtx, 0, *spec, &errorReportingRowReceiver{t})

			if err != nil {
				t.Fatalf("Could not create data processor: %v", err)
			}

			processor.Run(context.Background())
		})
	}
}

func TestImportHonorsResumePosition(t *testing.T) {
	defer leaktest.AfterTest(t)()

	batchSize := 13
	defer row.TestingSetDatumRowConverterBatchSize(batchSize)()

	// minKey used for the ensureMinKeyAdder bulk adder.
	var minKey roachpb.Key

	evalCtx := tree.MakeTestingEvalContext(nil)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings:        &cluster.Settings{},
			ExternalStorage: externalStorageFactory,
			BulkAdder: func(
				_ context.Context, _ *client.DB, _ hlc.Timestamp,
				_ storagebase.BulkAdderOptions) (storagebase.BulkAdder, error) {
				return &ensureMinKeyAdder{minKey: &minKey}, nil
			},
			TestingKnobs: execinfra.TestingKnobs{
				BulkAdderFlushesEveryBatch: true,
			},
		},
	}

	// In this test, we'll specify various resume positions for
	// different input formats. We expect that the rows before resume
	// position will be skipped. NB: We assume that the (external)
	// test files are sorted and contain at least batchSize rows.
	testSpecs := []testSpec{
		newTestSpec(t, roachpb.IOFileFormat_CSV, "testdata/csv/data-0"),
		newTestSpec(t, roachpb.IOFileFormat_Mysqldump, "testdata/mysqldump/simple.sql"),
		newTestSpec(t, roachpb.IOFileFormat_MysqlOutfile, "testdata/mysqlout/csv-ish/simple.txt"),
		newTestSpec(t, roachpb.IOFileFormat_PgCopy, "testdata/pgcopy/default/test.txt"),
		newTestSpec(t, roachpb.IOFileFormat_PgDump, "testdata/pgdump/simple.sql"),
	}
	resumes := []int64{0, 10, eofOffset}

	for _, testCase := range testSpecs {
		spec := testCase.getConverterSpec()

		for _, resumePos := range resumes {
			spec.ResumePos = map[int32]int64{0: resumePos}
			minKey = getPkeyForTable(t, spec.Tables["simple"].Desc, resumePos)

			t.Run(fmt.Sprintf("resume-%v-%v", spec.Format.Format, resumePos), func(t *testing.T) {
				rp := resumePos
				progCh := make(chan execinfrapb.RemoteProducerMetadata_BulkProcessorProgress)
				defer close(progCh)

				// Setup progress consumer.
				go func() {
					// Consume progress reports. Since we expect every batch to be flushed
					// (BulkAdderFlushesEveryBatch), then the progress resport must be emitted every
					// batchSize rows (possibly out of order), starting from our initial resumePos
					for prog := range progCh {
						if !t.Failed() && prog.ResumePos[0] < (rp+int64(batchSize)) {
							t.Logf("unexpected progress resume pos: %d", prog.ResumePos[0])
							t.Fail()
						}
					}
				}()

				_, err := runImport(context.Background(), flowCtx, spec, progCh)

				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// syncBarrier allows 2 threads (a controller and a worker) to
// synchronize between themselves. A controller portion of the
// barrier waits until worker starts running, and then notifies
// worker to proceed. The worker is the opposite: notifies controller
// that it started running, and waits for the proceed signal.
type syncBarrier interface {
	// Enter blocks the barrier, and returns a function
	// that, when executed, unblocks the other thread.
	Enter() func()
}

type barrier struct {
	read       <-chan struct{}
	write      chan<- struct{}
	controller bool
}

// Returns controller/worker barriers.
func newSyncBarrier() (syncBarrier, syncBarrier) {
	p1 := make(chan struct{})
	p2 := make(chan struct{})
	return &barrier{p1, p2, true}, &barrier{p2, p1, false}
}

func (b *barrier) Enter() func() {
	if b.controller {
		b.write <- struct{}{}
		return func() { <-b.read }
	}

	<-b.read
	return func() { b.write <- struct{}{} }
}

// A special jobs.Resumer that, instead of finishing
// the job successfully, forces the job to be paused.
var _ jobs.Resumer = &cancellableImportResumer{}

type cancellableImportResumer struct {
	ctx              context.Context
	jobIDCh          chan int64
	jobID            int64
	onSuccessBarrier syncBarrier
	wrapped          *importResumer
}

func (r *cancellableImportResumer) Resume(
	_ context.Context, phs interface{}, resultsCh chan<- tree.Datums,
) error {
	r.jobID = *r.wrapped.job.ID()
	r.jobIDCh <- r.jobID
	return r.wrapped.Resume(r.ctx, phs, resultsCh)
}

func (r *cancellableImportResumer) OnSuccess(ctx context.Context, txn *client.Txn) error {
	if r.onSuccessBarrier != nil {
		defer r.onSuccessBarrier.Enter()()
	}
	return errors.New("job succeed, but we're forcing it to be paused")
}

func (r *cancellableImportResumer) OnTerminal(
	ctx context.Context, status jobs.Status, resultsCh chan<- tree.Datums,
) {
	r.wrapped.OnTerminal(ctx, status, resultsCh)
}

func (r *cancellableImportResumer) OnFailOrCancel(ctx context.Context, txn *client.Txn) error {
	// This callback is invoked when an error or cancellation occurs
	// during the import. Since our OnSuccess handler returned an
	// error (after pausing the job), we need to short-circuits
	// jobs machinery so that this job is not marked as failed.
	return errors.New("bail out")
}

func setImportReaderParallelism(parallelism int32) func() {
	factory := rowexec.NewReadImportDataProcessor
	rowexec.NewReadImportDataProcessor = func(
		flowCtx *execinfra.FlowCtx, processorID int32,
		spec execinfrapb.ReadImportDataSpec, output execinfra.RowReceiver) (execinfra.Processor, error) {
		spec.ReaderParallelism = parallelism
		return factory(flowCtx, processorID, spec, output)
	}

	return func() {
		rowexec.NewReadImportDataProcessor = factory
	}
}

// Queries the status and the import progress of the job.
type jobState struct {
	err    error
	status jobs.Status
	prog   jobspb.ImportProgress
}

func queryJob(db sqlutils.DBHandle, jobID int64) (js jobState) {
	js = jobState{
		err:    nil,
		status: "",
		prog:   jobspb.ImportProgress{},
	}
	var progressBytes, payloadBytes []byte
	js.err = db.QueryRowContext(
		context.TODO(), "SELECT status, payload, progress FROM system.jobs WHERE id = $1", jobID).Scan(
		&js.status, &payloadBytes, &progressBytes)
	if js.err != nil {
		return
	}

	if js.status == jobs.StatusFailed {
		payload := &jobspb.Payload{}
		js.err = protoutil.Unmarshal(payloadBytes, payload)
		if js.err == nil {
			js.err = errors.New(payload.Error)
		}
		return
	}

	progress := &jobspb.Progress{}
	if js.err = protoutil.Unmarshal(progressBytes, progress); js.err != nil {
		return
	}
	js.prog = *(progress.Details.(*jobspb.Progress_Import).Import)
	return
}

// Repeatedly queries job status/progress until specified function returns true.
func queryJobUntil(
	t *testing.T, db sqlutils.DBHandle, jobID int64, isDone func(js jobState) bool,
) (js jobState) {
	for r := retry.Start(base.DefaultRetryOptions()); r.Next(); {
		js = queryJob(db, jobID)
		if js.err != nil || isDone(js) {
			break
		}
	}
	if js.err != nil {
		t.Fatal(js.err)
	}
	return
}

func TestCSVImportCanBeResumed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setImportReaderParallelism(1)()
	const batchSize = 5
	defer TestingSetCsvInputReaderBatchSize(batchSize)()
	defer row.TestingSetDatumRowConverterBatchSize(2 * batchSize)()
	jobs.DefaultAdoptInterval = 100 * time.Millisecond

	s, db, _ := serverutils.StartServer(t,
		base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					BulkAdderFlushesEveryBatch: true,
				},
			},
		})
	registry := s.JobRegistry().(*jobs.Registry)
	ctx := context.TODO()
	defer s.Stopper().Stop(ctx)

	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE DATABASE d`)
	sqlDB.Exec(t, "CREATE TABLE t (id INT, data STRING)")
	defer sqlDB.Exec(t, `DROP TABLE t`)

	jobCtx, cancelImport := context.WithCancel(ctx)
	jobIDCh := make(chan int64)
	var jobID int64 = -1
	var importSummary roachpb.BulkOpSummary

	registry.TestingResumerCreationKnobs = map[jobspb.Type]func(raw jobs.Resumer) jobs.Resumer{
		// Arrange for our special job resumer to be returned the very first time we start the import.
		jobspb.TypeImport: func(raw jobs.Resumer) jobs.Resumer {
			resumer := raw.(*importResumer)
			resumer.testingKnobs.alwaysFlushJobProgress = true
			resumer.testingKnobs.afterImport = func(summary roachpb.BulkOpSummary) error {
				importSummary = summary
				return nil
			}
			if jobID == -1 {
				return &cancellableImportResumer{
					ctx:     jobCtx,
					jobIDCh: jobIDCh,
					wrapped: resumer,
				}
			}
			return resumer
		},
	}

	testBarrier, csvBarrier := newSyncBarrier()
	csv1 := newCsvGenerator(0, 10*batchSize+1, &intGenerator{}, &strGenerator{})
	csv1.addBreakpoint(7*batchSize, func() (bool, error) {
		defer csvBarrier.Enter()()
		return false, nil
	})

	// Convince distsql to use our "external" storage implementation.
	storage := newGeneratedStorage(csv1)
	s.DistSQLServer().(*distsql.ServerImpl).ServerConfig.ExternalStorage = storage.externalStorageFactory()

	// Execute import; ignore any errors returned (since we're aborting the first import run.).
	go func() {
		_, _ = sqlDB.DB.ExecContext(ctx,
			`IMPORT INTO t (id, data) CSV DATA ($1)`, storage.getGeneratorURIs()[0])
	}()

	// Wait for the job to start running
	jobID = <-jobIDCh

	// Wait until we are blocked handling breakpoint.
	unblockImport := testBarrier.Enter()
	// Wait until we have recorded some job progress.
	js := queryJobUntil(t, sqlDB.DB, jobID, func(js jobState) bool { return js.prog.ResumePos[0] > 0 })

	// Pause the job;
	if err := registry.Pause(ctx, nil, jobID); err != nil {
		t.Fatal(err)
	}
	// Send cancellation and unblock breakpoint.
	cancelImport()
	unblockImport()

	// Get updated resume position counter.
	js = queryJobUntil(t, sqlDB.DB, jobID, func(js jobState) bool { return jobs.StatusPaused == js.status })
	resumePos := js.prog.ResumePos[0]
	t.Logf("Resume pos: %v\n", js.prog.ResumePos[0])

	// Resume the job and wait for it to complete.
	if err := registry.Resume(ctx, nil, jobID); err != nil {
		t.Fatal(err)
	}
	js = queryJobUntil(t, sqlDB.DB, jobID, func(js jobState) bool { return jobs.StatusSucceeded == js.status })

	// Verify that the import proceeded from the resumeRow position.
	assert.Equal(t, importSummary.Rows, int64(csv1.numRows)-resumePos)

	sqlDB.CheckQueryResults(t, `SELECT id FROM t ORDER BY id`,
		sqlDB.QueryStr(t, `SELECT generate_series(0, $1)`, csv1.numRows-1),
	)
}

func TestCSVImportMarksFilesFullyProcessed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const batchSize = 5
	defer TestingSetCsvInputReaderBatchSize(batchSize)()
	defer row.TestingSetDatumRowConverterBatchSize(2 * batchSize)()
	jobs.DefaultAdoptInterval = 100 * time.Millisecond

	s, db, _ := serverutils.StartServer(t,
		base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					BulkAdderFlushesEveryBatch: true,
				},
			},
		})
	registry := s.JobRegistry().(*jobs.Registry)
	ctx := context.TODO()
	defer s.Stopper().Stop(ctx)

	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE DATABASE d`)
	sqlDB.Exec(t, "CREATE TABLE t (id INT, data STRING)")
	defer sqlDB.Exec(t, `DROP TABLE t`)

	jobIDCh := make(chan int64)
	controllerBarrier, importBarrier := newSyncBarrier()

	var jobID int64 = -1
	var importSummary roachpb.BulkOpSummary

	registry.TestingResumerCreationKnobs = map[jobspb.Type]func(raw jobs.Resumer) jobs.Resumer{
		// Arrange for our special job resumer to be
		// returned the very first time we start the import.
		jobspb.TypeImport: func(raw jobs.Resumer) jobs.Resumer {
			resumer := raw.(*importResumer)
			resumer.testingKnobs.alwaysFlushJobProgress = true
			resumer.testingKnobs.afterImport = func(summary roachpb.BulkOpSummary) error {
				importSummary = summary
				return nil
			}
			if jobID == -1 {
				return &cancellableImportResumer{
					ctx:              ctx,
					jobIDCh:          jobIDCh,
					onSuccessBarrier: importBarrier,
					wrapped:          resumer,
				}
			}
			return resumer
		},
	}

	csv1 := newCsvGenerator(0, 10*batchSize+1, &intGenerator{}, &strGenerator{})
	csv2 := newCsvGenerator(0, 20*batchSize-1, &intGenerator{}, &strGenerator{})
	csv3 := newCsvGenerator(0, 1, &intGenerator{}, &strGenerator{})

	// Convince distsql to use our "external" storage implementation.
	storage := newGeneratedStorage(csv1, csv2, csv3)
	s.DistSQLServer().(*distsql.ServerImpl).ServerConfig.ExternalStorage = storage.externalStorageFactory()

	// Execute import; ignore any errors returned
	// (since we're aborting the first import run).
	go func() {
		_, _ = sqlDB.DB.ExecContext(ctx,
			`IMPORT INTO t (id, data) CSV DATA ($1, $2, $3)`, storage.getGeneratorURIs()...)
	}()

	// Wait for the job to start running
	jobID = <-jobIDCh

	// Tell importer that it can continue with it's onSuccess
	proceedImport := controllerBarrier.Enter()

	// Pause the job;
	if err := registry.Pause(ctx, nil, jobID); err != nil {
		t.Fatal(err)
	}

	// All files should have been processed,
	// and the resume position set to maxInt64.
	js := queryJobUntil(t, sqlDB.DB, jobID, func(js jobState) bool { return jobs.StatusPaused == js.status })
	for _, pos := range js.prog.ResumePos {
		assert.True(t, pos == math.MaxInt64)
	}

	// Send cancellation and unblock import.
	proceedImport()

	// Resume the job and wait for it to complete.
	if err := registry.Resume(ctx, nil, jobID); err != nil {
		t.Fatal(err)
	}
	js = queryJobUntil(t, sqlDB.DB, jobID, func(js jobState) bool { return jobs.StatusSucceeded == js.status })

	// Verify that after resume we have not processed any additional rows.
	assert.Zero(t, importSummary.Rows)
}

func (ses *generatedStorage) externalStorageFactory() cloud.ExternalStorageFactory {
	return func(_ context.Context, es roachpb.ExternalStorage) (cloud.ExternalStorage, error) {
		uri, err := url.Parse(es.HttpPath.BaseUri)
		if err != nil {
			return nil, err
		}
		id, ok := ses.nameIDMap[uri.Path]
		if !ok {
			id = ses.nextID
			ses.nextID++
			ses.nameIDMap[uri.Path] = id
		}
		return &generatorExternalStorage{conf: es, gen: ses.generators[id]}, nil
	}
}

// External storage factory needed to run converters.
func externalStorageFactory(
	ctx context.Context, dest roachpb.ExternalStorage,
) (cloud.ExternalStorage, error) {
	workdir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return cloud.MakeExternalStorage(ctx, dest, nil, blobs.TestBlobServiceClient(workdir))
}

// Helper to create and initialize testSpec.
func newTestSpec(
	t *testing.T, inputFormat roachpb.IOFileFormat_FileFormat, inputs ...string,
) testSpec {
	spec := testSpec{
		inputFormat: inputFormat,
		inputs:      make(map[int32]string),
	}

	// Initialize table descriptor for import. We need valid descriptor to run
	// converters, even though we don't actually import anything in this test.
	var descr *sqlbase.TableDescriptor
	switch inputFormat {
	case roachpb.IOFileFormat_CSV:
		descr = descForTable(t,
			"CREATE TABLE simple (i INT PRIMARY KEY, s text )", 10, 20, NoFKs)
	case roachpb.IOFileFormat_Mysqldump, roachpb.IOFileFormat_MysqlOutfile,
		roachpb.IOFileFormat_PgDump, roachpb.IOFileFormat_PgCopy:
		descr = descForTable(t,
			"CREATE TABLE simple (i INT PRIMARY KEY, s text, b bytea default null)", 10, 20, NoFKs)
	default:
		t.Fatalf("Unsupported input format: %v", inputFormat)
	}

	targetCols := make([]string, len(descr.Columns))
	numCols := 0
	for i, col := range descr.Columns {
		if !col.Hidden {
			targetCols[i] = col.Name
			numCols++
		}
	}
	assert.True(t, numCols > 0)

	spec.tables = map[string]*execinfrapb.ReadImportDataSpec_ImportTable{
		"simple": {Desc: descr, TargetCols: targetCols[0:numCols]},
	}

	for id, path := range inputs {
		spec.inputs[int32(id)] = cloud.MakeLocalStorageURI(path)
	}

	return spec
}

func getPkeyForTable(t *testing.T, descr *sqlbase.TableDescriptor, id int64) roachpb.Key {
	colMap := make(map[sqlbase.ColumnID]int, len(descr.Columns))
	for i, col := range descr.Columns {
		colMap[col.ID] = i
	}

	primaryIndexKeyPrefix := sqlbase.MakeIndexKeyPrefix(descr, descr.PrimaryIndex.ID)
	pk, _, err := sqlbase.EncodeIndexKey(
		descr, &descr.PrimaryIndex, colMap,
		[]tree.Datum{tree.NewDInt(tree.DInt(id))}, primaryIndexKeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}
