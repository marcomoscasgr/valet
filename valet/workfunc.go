/*
 * Copyright (C) 2019, 2020, 2021, 2022. Genome Research Ltd. All rights
 * reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License,
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * @file workfunc.go
 * @author Keith James <kdj@sanger.ac.uk>
 */

package valet

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/pgzip"
	"github.com/pkg/errors"
	ex "github.com/wtsi-npg/extendo/v2"

	"github.com/wtsi-npg/valet/utilities"

	logs "github.com/wtsi-npg/logshim"
)

// WorkFunc is a worker function used by DoProcessFiles.
type WorkFunc func(path FilePath) error

// Work describes a function to be executed and the rank of the execution. When
// there is a choice of Work to be executed, Work with the smallest Rank value
// (i.e. the highest rank) is performed first. In the case of a tie, either Work
// may be selected for execution.
type Work struct {
	WorkFunc WorkFunc // A WorkFunc to execute
	Rank     uint16   // The rank of the work
}

// WorkArr is a series of Work to be executed in ascending rank order.
type WorkArr []Work

func (s WorkArr) Len() int {
	return len(s)
}

func (s WorkArr) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s WorkArr) Less(i, j int) bool {
	return s[i].Rank < s[j].Rank
}

func (s WorkArr) IsEmpty() bool {
	return len(s) == 0
}

// WorkMatch is an association between a FilePredicate and Work to be done. If
// the predicate returns true then the work will be done.
type WorkMatch struct {
	pred    FilePredicate // Predicate to match against candidate FilePath
	work    Work          // Work to be executed on a matching FilePath
	predDoc string        // A short description of the match criteria
	workDoc string        // A short description of the work
}

const OxfordNanoporeNamespace string = "ont"

// String returns a descriptive string for the WorkMatch which includes the
// predicate and work documentation strings.
func (m WorkMatch) String() string {
	return fmt.Sprintf("%s => %s", m.predDoc, m.workDoc)
}

// WorkPlan is a slice of WorkMatches. Where more than one Work is matched,
// they will be done in rank order.
type WorkPlan []WorkMatch

func (p WorkPlan) Len() int {
	return len(p)
}

func (p WorkPlan) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func (p WorkPlan) Less(i, j int) bool {
	return p[i].work.Rank < p[j].work.Rank
}

func (p WorkPlan) IsEmpty() bool {
	return len(p) == 0
}

func (p WorkPlan) String() string {
	b := strings.Builder{}
	b.WriteString("[")
	for i, m := range p {
		b.WriteString(m.String())
		if i < len(p)-1 {
			b.WriteString(",")
		}
	}
	b.WriteString("]")

	return b.String()
}

// DryRunWorkPlan matches any FilePath and does DoNothing Work.
func DryRunWorkPlan() WorkPlan {
	return []WorkMatch{{
		pred:    IsTrue,
		predDoc: "Is True",
		work:    Work{WorkFunc: DoNothing},
		workDoc: "Do Nothing"}}
}

// CreateChecksumWorkPlan manages checksum files.
func CreateChecksumWorkPlan() WorkPlan {
	return []WorkMatch{{
		pred:    RequiresChecksum,
		predDoc: "Requires Local Checksum File",
		work:    Work{WorkFunc: CreateOrUpdateMD5ChecksumFile},
		workDoc: "Create Or Update Local MD5 Checksum File"}}
}

// ChecksumStateWorkPlan counts files that do not have a checksum.
func ChecksumStateWorkPlan(countFunc WorkFunc) WorkPlan {
	return []WorkMatch{{
		pred:    RequiresChecksum,
		predDoc: "Requires Local Checksum File",
		work:    Work{WorkFunc: countFunc},
		workDoc: "Count File"}}
}

// RemoveDirectoryWorkPlan removes empty work directories that are older than
// the specified duration.
func RemoveDirectoryWorkPlan(duration time.Duration) WorkPlan {
	return []WorkMatch{{
		pred:    MakeRequiresRemoval(duration),
		predDoc: "Requires Removal",
		work:    Work{WorkFunc: RemoveDirectory},
		workDoc: "Remove Old Run Folder",
	}}
}

// ArchiveFilesWorkPlan copies files and metadata to iRODS via the following
// steps:
//
// 1. Compresses local files where needed
// 2. Creates or updated checksum files
// 3. Copies files to iRODS
// 4. Annotates metadata in iRODS
//
// Additional steps are done if deleteLocal is true:
//
// 5. Uncompressed copies of local compressed files are removed
// 6. Successfully archived local files are removed
// 7. Redundant local checksum files are removed
// 8. Empty run directories are removed, after a delay
func ArchiveFilesWorkPlan(localBase string, remoteBase string,
	cPool *ex.ClientPool, deleteLocal bool, cleanup time.Duration) WorkPlan {

	copyFile := MakeCopier(localBase, remoteBase, cPool)
	isCopied := MakeIsCopied(localBase, remoteBase, cPool)

	annotateFile := MakeAnnotator(localBase, remoteBase, cPool)
	isAnnotated := MakeIsAnnotated(localBase, remoteBase, cPool)

	// The isCopied test expects an MD5 file to be present and will raise an
	// error if not (and MD5 is essential). The RequiresCopying test is applied
	// first to avoid errors on files that are just being compressed by this
	// plan, prior to a later call to this same WorkPlan to do the archiving.
	isArchived := Or(
		And(RequiresCopying, isCopied, And(RequiresAnnotation, isAnnotated)),
		And(RequiresCopying, isCopied, Not(RequiresAnnotation)))

	hasRedundantChecksumFile := Or(
		And(Not(RequiresCopying), HasChecksumFile), // E.g. fastq
		And(RequiresCopying, isCopied, HasChecksumFile))

	requiresRemoval := MakeRequiresRemoval(cleanup)

	// Currently the entire processing pipeline is launched with a single
	// WorkPlan as a parameter. All files passing the filters are operated on
	// according to that plan.
	//
	// TODO: Maybe a choice of WorkPlans at runtime?

	plan := []WorkMatch{
		{
			pred:    RequiresCompression,
			predDoc: "Requires Compression Locally",
			work:    Work{WorkFunc: CompressFile, Rank: 1},
			workDoc: "Compress Local File",
		},
		{
			pred:    RequiresChecksum,
			predDoc: "Requires Local Checksum File",
			work:    Work{WorkFunc: CreateOrUpdateMD5ChecksumFile, Rank: 2},
			workDoc: "Create Or Update Local MD5 Checksum File",
		},
		{
			pred:    And(RequiresCopying, Not(isCopied)),
			predDoc: "Requires Copying && Is Not Copied",
			work:    Work{WorkFunc: copyFile, Rank: 3},
			workDoc: "Archive",
		},
		{
			pred:    And(RequiresAnnotation, Not(isAnnotated)),
			predDoc: "Requires Annotation && Is Not Annotated",
			work:    Work{WorkFunc: annotateFile, Rank: 4},
			workDoc: "Archive",
		},
	}

	if deleteLocal {
		plan = append(plan,
			WorkMatch{
				pred:    HasCompressedVersion,
				predDoc: "Has Local Compressed Version",
				work:    Work{WorkFunc: RemoveFile, Rank: 5},
				workDoc: "Remove Local Uncompressed Version",
			},
			WorkMatch{
				pred:    isArchived,
				predDoc: "Requires Archiving && Is Archived",
				work:    Work{WorkFunc: RemoveFile, Rank: 6},
				workDoc: "Remove Local File",
			},
			WorkMatch{
				// A checksum file for a file that has been archived
				// successfully or a file that is not to be being archived can
				// be cleaned up.
				pred:    hasRedundantChecksumFile,
				predDoc: "Has Local Checksum File No Longer Needed",
				work:    Work{WorkFunc: RemoveMD5ChecksumFile, Rank: 7},
				workDoc: "Remove Local MD5 Checksum File",
			},
			WorkMatch{
				pred:    requiresRemoval,
				predDoc: "Requires Removal",
				work:    Work{WorkFunc: RemoveDirectory, Rank: 8},
				workDoc: "Remove Old Run Directory",
			})
	}

	return plan
}

// DoNothing does nothing apart from log at debug level that it has been
// called. It is used to implement dry-run operations.
func DoNothing(path FilePath) error {
	logs.GetLogger().Debug().
		Str("path", path.Location).Msg("would work on this")
	return nil
}

// CreateOrUpdateMD5ChecksumFile calculates a checksum for the file at path and
// writes it to a new checksum file as a hex-encoded string. This function only
// operates when there is no existing checksum file, or when the existing
// checksum file is stale (its last modified time is older than the last
// modified time of path). If the checksum file is stale this function deletes
// it before creating a new one.
func CreateOrUpdateMD5ChecksumFile(path FilePath) error {
	fn := "CreateOrUpdateMD5ChecksumFile"

	staleFile, err := HasStaleChecksumFile(path)
	if err != nil {
		return errors.Wrap(err, fn)
	}

	if staleFile {
		return UpdateMD5ChecksumFile(path)
	}

	hasFile, err := HasChecksumFile(path)
	if err != nil {
		return errors.Wrap(err, fn)
	}

	if !hasFile {
		err = CreateMD5ChecksumFile(path)
		if err != nil {
			return errors.Wrap(err, fn)
		}
	}

	return nil
}

// CreateMD5ChecksumFile calculates a checksum file for the data file at path
// with contents as a hex-encoded string. It raises an error if the checksum
// file already exists.
func CreateMD5ChecksumFile(path FilePath) error {
	md5sum, err := CalculateFileMD5(path)
	if err != nil {
		return errors.Wrap(err, "CreateMD5ChecksumFile")
	}

	return createMD5File(path.ChecksumFilename(), md5sum)
}

// UpdateMD5ChecksumFile removes the existing checksum file, if it exists and
// creates a new one.
func UpdateMD5ChecksumFile(path FilePath) error {
	fn := "UpdateMD5ChecksumFile"
	if rerr := RemoveMD5ChecksumFile(path); rerr != nil {
		return errors.Wrap(rerr, fn)
	}

	log := logs.GetLogger()
	log.Debug().Str("path", path.Location).
		Msg("removed stale MD5 file")

	if cerr := CreateMD5ChecksumFile(path); cerr != nil {
		log.Error().Err(cerr).
			Str("path", path.Location).
			Msg("failed to create a new MD5 file")
		return errors.Wrap(cerr, fn)
	}

	return nil
}

// RemoveMD5ChecksumFile removes the MD5 checksum file corresponding to path.
// If the file does not exist by the time removal is attempted, no error is
// raised.
func RemoveMD5ChecksumFile(path FilePath) error {
	err := os.Remove(path.ChecksumFilename())
	if os.IsNotExist(err) {
		return nil
	}
	return errors.Wrap(err, "RemoveMD5ChecksumFile")
}

// CompressFile compresses the target file using gzip. While doing so, it tee's
// both the uncompressed data and compressed data to make MD5 checksums of
// these and writes checksum files for the original, uncompressed file and the
// new compressed file.
func CompressFile(path FilePath) (err error) { // NRV
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "CompressFile")
		}
	}()

	var in *os.File
	if in, err = os.Open(path.Location); err != nil {
		return
	}

	defer func() {
		err = utilities.CombineErrors(err, in.Close())
	}()

	// We use temp file and rename to add the compressed file to the data
	// directory
	var tmp *os.File
	if tmp, err = ioutil.TempFile(os.TempDir(), "valet-"); err != nil {
		return
	}

	defer func() {
		// Clean up if we got this far and the temp file still exists
		if rerr := os.Remove(tmp.Name()); !os.IsNotExist(rerr) {
			err = utilities.CombineErrors(err, rerr)
		}
	}()

	outPath := path.CompressedFilename()
	log := logs.GetLogger()
	log.Debug().Str("src", path.Location).
		Str("to", outPath).Msg("compressing")

	hCmp := md5.New()
	mwCmp := io.MultiWriter(hCmp, tmp) // Write to MD5 and output file
	gzw := pgzip.NewWriter(mwCmp)

	hRaw := md5.New()
	mwRaw := io.MultiWriter(hRaw, gzw) // Write to MD5 and compressor

	if _, err = io.Copy(mwRaw, in); err != nil {
		return
	}
	if err = gzw.Close(); err != nil {
		return
	}
	if err = tmp.Close(); err != nil {
		return
	}
	if err = os.Rename(tmp.Name(), outPath); err != nil {
		return
	}

	// We can make a checksum file for the compressed data right away. This
	// must be done after the compressed file is in position.
	var outFile FilePath
	outFile, err = NewFilePath(outPath)
	md5Cmp := hCmp.Sum(nil)
	if err = createMD5File(outFile.ChecksumFilename(), md5Cmp); err != nil {
		return
	}

	// We can also make a checksum file for the raw data
	md5Raw := hRaw.Sum(nil)
	if err = createMD5File(path.ChecksumFilename(), md5Raw); err != nil {
		return
	}

	log.Debug().Str("src", path.Location).
		Str("checksum_raw", fmt.Sprintf("%x", md5Raw)).
		Str("checksum", fmt.Sprintf("%x", md5Cmp)).
		Str("to", outPath).Msg("compressed")

	return
}

// CalculateFileMD5 returns the MD5 checksum of the file at path.
func CalculateFileMD5(path FilePath) (md5sum []byte, err error) { // NRV
	var f *os.File
	if f, err = os.Open(path.Location); err != nil {
		return
	}

	defer func() {
		err = utilities.CombineErrors(err, f.Close())
	}()

	h := md5.New()
	if _, err = io.Copy(h, f); err != nil {
		return
	}
	md5sum = h.Sum(nil)
	return
}

// ReadMD5ChecksumFile reads and returns a checksum from a local file created by
// CreateMD5ChecksumFile. It trims any whitespace (including any newline) from
// the beginning and end of the checksum.
func ReadMD5ChecksumFile(path FilePath) (md5sum []byte, err error) { // NRV
	var f *os.File
	if f, err = os.Open(path.Location); err != nil {
		return
	}

	defer func() {
		err = utilities.CombineErrors(err, f.Close())
	}()

	md5sum, err = bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return
	}
	md5sum = bytes.TrimSpace(md5sum)

	return
}

// AddMinKNOWReportAnnotation adds annotation from report to the parent
// collection of the archived report obj.
func AddMinKNOWReportAnnotation(obj *ex.DataObject, report MinKNOWReport) error {
	meta, err := report.AsEnhancedMetadata()
	if err != nil {
		return err
	}

	return obj.Parent().ReplaceMetadata(meta)
}

// MakeCopier returns a WorkFunc capable of copying files to iRODS. Each
// file passed to the WorkFunc will have its path relative to localBase
// calculated. This relative path will then be appended to remoteBase to give
// the full destination path in iRODS. E.g.
//
// localBase        = /a/b/c
// remoteBase       = /zone1/x/y
//
// file path        = /a/b/c/d/e/f.fast5
//
// therefore:
//
// relative path    = ./d/e/f.txt
// destination path = /zone1/x/y/d/e/f.fast5
//
// Any leading iRODS collections will be created by the WorkFunc as required.
//
// WorkFunc prerequisites: CreateOrUpdateMD5ChecksumFile
//
// i.e. files for copying are expected to have an MD5 checksum file.
func MakeCopier(localBase string, remoteBase string,
	cPool *ex.ClientPool) WorkFunc {

	return func(path FilePath) (err error) { // NRV
		var dst string
		dst, err = translatePath(localBase, remoteBase, path)

		var chkFile FilePath
		chkFile, err = NewFilePath(path.ChecksumFilename())
		if err != nil {
			return
		}

		var checksum []byte
		checksum, err = ReadMD5ChecksumFile(chkFile)

		log := logs.GetLogger()
		log.Debug().Str("src", path.Location).Str("to", dst).
			Str("checksum", string(checksum)).Msg("archiving")

		var client *ex.Client
		if client, err = cPool.Get(); err != nil {
			return
		}

		defer func() {
			err = utilities.CombineErrors(err, cPool.Return(client))
		}()

		coll := ex.NewCollection(client, filepath.Dir(dst))
		if err = coll.Ensure(); err != nil {
			return
		}

		chk := string(checksum)
		if _, err = ex.ArchiveDataObject(client, path.Location, dst, chk,
			ex.MakeCreationMetadata(chk)); err != nil {
			return
		}

		log.Debug().Str("path", path.Location).Str("to", dst).
			Str("checksum", string(checksum)).Msg("archived")
		return
	}
}

// MakeAnnotator returns a WorkFunc that will add to iRODS any annotation
// associated with local files. Each file passed to the WorkFunc will be
// examined to see if has associated metadata e.g. it might contain metadata
// itself, or be somehow linked to some metadata. Any relevant metadata will
// be copied to iRODS e.g. it might be added to the file's data object in
// iRODS, or to some other data object or collection.
//
// The capabilities are listed below:
//
// -  MinKNOW report files.
//
//    The metadata contained in MinKNOW report files is parsed abd added to the
//    collection containing the report data object in iRODS.
func MakeAnnotator(localBase string, remoteBase string,
	cPool *ex.ClientPool) WorkFunc {

	return func(path FilePath) (err error) { // NRV
		var dst string
		dst, err = translatePath(localBase, remoteBase, path)

		var client *ex.Client
		if client, err = cPool.Get(); err != nil {
			return
		}

		defer func() {
			err = utilities.CombineErrors(err, cPool.Return(client))
		}()

		var isReport bool
		isReport, err = IsMinKNOWReport(path)
		if err != nil {
			return
		}

		if isReport {
			var report MinKNOWReport
			report, err = ParseMinKNOWReport(path.Location)
			if err != nil {
				return
			}

			obj := ex.NewDataObject(client, dst)
			err = AddMinKNOWReportAnnotation(obj, report)
			if err != nil {
				return
			}
		}
		return
	}
}

// RemoveFile removes the specified file.
func RemoveFile(path FilePath) error {
	log := logs.GetLogger()

	log.Debug().Str("path", path.Location).Msg("deleting")

	err := os.Remove(path.Location)
	if os.IsNotExist(err) {
		log.Warn().Str("path", path.Location).
			Msg("had gone before deletion")
		return nil
	}

	return err
}

// RemoveDirectory removes directories under a root, recursively. It skips any
// that contain files, or whose descendants contain files.
func RemoveDirectory(path FilePath) error {
	log := logs.GetLogger()
	log.Info().Str("path", path.Location).Msg("safe deleting recursively")

	if !path.Info.IsDir() {
		return errors.Errorf("Failed to remove %s as it is not a directory",
			path.Location)
	}

	safeRemoveDir := func(p string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			return nil
		}

		contents, err := os.ReadDir(p)
		if err != nil {
			return err
		}

		for _, e := range contents {
			if !e.IsDir() {
				log.Warn().
					Str("path", p).
					Msgf("file remains in run directory: %s", e.Name())
			}
		}

		if len(contents) != 0 {
			return nil
		}

		err = os.Remove(p)
		if os.IsNotExist(err) {
			log.Warn().Str("path", p).
				Msg("had gone before deletion")
			return nil
		}

		return err
	}

	return filepath.Walk(path.Location, safeRemoveDir)
}

// makeWork accepts a candidate FilePath and a WorkPlan and returns Work
// encapsulating all the work in the WorkPlan. If no work is required for the
// FilePath, it returns DoNothing Work.
//
// All predicates are evaluated as any work is done, therefore if some
// predicates are true only after earlier work in the WorkPlan is complete,
// they will pass, provided work is ranked in the appropriate order.
func makeWork(path FilePath, plan WorkPlan) (Work, error) {
	if plan.IsEmpty() {
		return Work{WorkFunc: DoNothing}, nil
	}

	workFunc := func(fp FilePath) error {
		wp := plan
		sort.Sort(wp)

		log := logs.GetLogger()

		for _, wm := range wp {
			ok, err := wm.pred(fp)
			if err != nil {
				return err
			}

			if ok {
				log.Info().Str("path", fp.Location).
					Str("desc", wm.String()).
					Uint64("rank", uint64(wm.work.Rank)).
					Msg("working")

				if err := wm.work.WorkFunc(fp); err != nil {
					return err
				}
			} else {
				log.Debug().Str("path", path.Location).
					Str("desc", wm.String()).
					Uint64("rank", uint64(wm.work.Rank)).
					Msg("no match, ignoring work")
			}
		}

		return nil
	}

	return Work{WorkFunc: workFunc}, nil
}

func createMD5File(path string, md5sum []byte) (err error) { // NRV
	var f *os.File
	if f, err = ioutil.TempFile(os.TempDir(), "valet-"); err != nil {
		return
	}

	_, err = f.WriteString(fmt.Sprintf("%x\n", md5sum))
	if err = f.Close(); err != nil {
		return
	}

	defer func() {
		// Clean up if we got this far and the temp file still exists
		if rerr := os.Remove(f.Name()); !os.IsNotExist(rerr) {
			err = utilities.CombineErrors(err, rerr)
		}
	}()

	err = os.Rename(f.Name(), path)

	return
}

func translatePath(lBase string, rBase string, path FilePath) (string, error) {
	src, err := filepath.Rel(lBase, path.Location)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(rBase, src)), err
}
