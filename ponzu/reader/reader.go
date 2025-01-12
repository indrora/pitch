package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/davecgh/go-spew/spew"
	"github.com/indrora/ponzu/ponzu/format"
	"github.com/indrora/ponzu/ponzu/ioutil"
	"golang.org/x/crypto/blake2b"
)

// The reader is much simpler than the writer.

type ReaderState int

var (
	ErrExpectedHeader    = errors.New("expected a header, got something else")
	ErrUnexpectedData    = errors.New("unexpected data length for record type")
	ErrExpectedContinue  = errors.New("expected continue, got other")
	ErrUnexpectedControl = errors.New("unexpected control message")
	ErrUnknownRecordType = errors.New("unknown record type")
	ErrHashMismatch      = errors.New("hash does not match")
	ErrState             = errors.New("tried reading body before you got a header")
	ErrWalk              = errors.New("walk function returned non-nil error")
)

const (
	STATE_EMPTY ReaderState = 0
	STATE_OK    ReaderState = 1
	STATE_BODY  ReaderState = 2
)

type Reader struct {
	stream       *ioutil.BlockReader
	lastPreamble *format.Preamble

	zstdDict []byte
}

func NewReader(reader io.Reader) *Reader {
	return &Reader{
		stream:       ioutil.NewBlockReader(reader, format.BLOCK_SIZE),
		lastPreamble: nil,
	}
}

func (reader *Reader) Next() (*format.Preamble, interface{}, error) {

	if reader.lastPreamble != nil {
		// we have a previous header!
		// exhaust any data
		reader.CopyTo(io.Discard, false)
		reader.stream.Realign()
		reader.lastPreamble = nil

	}

	var err error

	mPreamble := &format.Preamble{}

	if err = binary.Read(reader.stream, binary.BigEndian, mPreamble); err != nil {
		return nil, nil, errors.Join(err, ErrExpectedHeader)
	}

	// verify preamble magic

	if !bytes.Equal(mPreamble.Magic[:], format.PREAMBLE_BYTES[:]) {
		spew.Dump(mPreamble)
		return nil, nil, ErrExpectedHeader
	}

	// Parse from the preamble the metadata.
	cborData := new(bytes.Buffer)
	n, err := io.CopyN(cborData, reader.stream, int64(mPreamble.MetadataLength))

	// Realign the reader to the start of the data (or next record)
	reader.stream.Realign()

	if n != int64(mPreamble.MetadataLength) {
		return mPreamble, nil, fmt.Errorf("%w: tried reading %v bytes, only got %v of metadata", err, mPreamble.MetadataLength, n)
	} else if err != nil {
		return mPreamble, nil, err
	}

	cborDataBytes := cborData.Bytes()
	metaHashCheck := blake2b.Sum512(cborDataBytes)

	if !bytes.Equal(metaHashCheck[:], mPreamble.MetadataChecksum[:]) {
		return mPreamble, nil, fmt.Errorf("%w: metadata checksum failed, expected %x, got %x ", ErrHashMismatch, mPreamble.MetadataChecksum, metaHashCheck)
	}

	var metadata any = nil

	if len(cborDataBytes) > 0 {
		metadata = unmarshalMetadata(mPreamble, cborDataBytes)
	}

	reader.lastPreamble = mPreamble

	switch mPreamble.Rtype {

	case format.RECORD_TYPE_CONTROL:
	case format.RECORD_TYPE_DIRECTORY:
	case format.RECORD_TYPE_HARDLINK:
	case format.RECORD_TYPE_SYMLINK:
	case format.RECORD_TYPE_OS_SPECIAL:
		if mPreamble.DataLen != 0 {
			return mPreamble, metadata, fmt.Errorf("%w: expected 0, got %v", ErrUnexpectedData, mPreamble.DataLen)
		}
	case format.RECORD_TYPE_ZDICTIONARY:
		// Special case: we are going to consume the zstd dictionary and then return the next frame afterwards
		spew.Dump(mPreamble)
		buff := new(bytes.Buffer)
		fmt.Println("calling CopyAll")
		err := reader.CopyAll(buff, true)
		if err != nil && err != io.EOF {
			fmt.Println("Failed to copy all data for zstd dictionary:", err)
			return nil, nil, err
		} else {
			reader.zstdDict = buff.Bytes()
			return reader.Next()
		}

	default:

	}

	return mPreamble, metadata, nil

}

func (reader *Reader) HasBody() bool {
	if reader.lastPreamble != nil {
		return reader.lastPreamble.DataLen != 0
	} else {
		return false
	}
}

func (reader *Reader) Validate() (bool, error) {
	if !reader.HasBody() {
		return true, nil
	}

	// Hack: since the checksum is calculated after compression, we don't care.
	reader.lastPreamble.Compression = format.COMPRESSION_NONE

	err := reader.CopyAll(io.Discard, true)
	if err == ErrHashMismatch {
		return false, err
	}
	// CopyTo returns io.EOF when
	if err != io.EOF {
		return false, err
	}
	return true, nil
}

func (reader *Reader) GetBody(validate bool) ([]byte, error) {

	// if there is no body, we clean up the header and leave.

	// decompress it into the appropriate buffer.
	body := new(bytes.Buffer)

	err := reader.CopyTo(body, validate)

	if err != nil && err != io.EOF {
		return nil, err
	}

	err2 := reader.stream.Realign()
	return body.Bytes(), errors.Join(err, err2)
}

func (reader *Reader) CopyTo(writer io.Writer, validate bool) error {

	// if there is no body, we clean up the header and leave.

	var err error

	if !reader.HasBody() {

		reader.lastPreamble = nil
		return io.EOF

	}

	// Otherwise, we're going to fill up our buffer.

	bodyLen := (reader.lastPreamble.DataLen * format.BLOCK_SIZE)
	if reader.lastPreamble.Modulo != 0 {
		bodyLen = bodyLen - (format.BLOCK_SIZE - uint64(reader.lastPreamble.Modulo))
	}

	// Get a limited reader
	dataReader := io.LimitReader(reader.stream, int64(bodyLen))

	// set up the tee: This allows us to compute the checksum in-situ, while the read is happening
	// at no performance penalty.
	hash, _ := blake2b.New512(nil)
	// tee from the limited reader to the hash function glub glub
	tee := io.TeeReader(dataReader, hash)
	// Wrap it in our decompression function (in the simple case, this is null, otherwise this is a zstd/brotli decompressor)
	dataReader, err = reader.getDecompressor(tee, reader.lastPreamble.Compression)

	if err != nil {
		return err
	}

	_, err = io.Copy(writer, dataReader)

	if err != nil && err != io.EOF {
		// something terrible has happened.
		return err

	}

	checksum := hash.Sum(nil)

	alignerr := reader.stream.Realign()
	// if we've been asked to validate the checksum, do it now

	if validate {
		if !bytes.Equal(checksum, reader.lastPreamble.DataChecksum[:]) {
			reader.lastPreamble = nil
			return ErrHashMismatch
		}
	}

	reader.lastPreamble = nil
	return errors.Join(err, alignerr)
}

func (reader *Reader) CopyAll(writer io.Writer, validate bool) error {
	fmt.Println("xxx: copyAll")
more:

	if reader.lastPreamble == nil {
		return io.ErrUnexpectedEOF
	}

	continues := reader.lastPreamble.Flags&format.RECORD_FLAG_CONTINUES == format.RECORD_FLAG_CONTINUES

	err := reader.CopyTo(writer, validate)
	fmt.Println("xxx: copy err: ", err)
	if err != nil && err != io.EOF {
		return err
	}

	if continues {
		tPre, _, err := reader.Next()
		if err != nil {
			return err
		} else if tPre.Rtype != format.RECORD_TYPE_CONTINUE || tPre.Flags&format.RECORD_FLAG_CONTINUES == 0 {
			return ErrExpectedContinue
		}
		goto more
	}

	return nil
}

/*
*
The WalkFunc controls the walk. Passed to it are the same results from `Reader.Next`, minus any errors.

If it returns a non-nil error, the error is wrapped by a WalkError.
*/
type WalkFunc func(*format.Preamble, any) error

// Walk through the
func (r *Reader) Walk(f WalkFunc) error {

	var err error = nil

	for !errors.Is(err, io.EOF) {

		var preamble *format.Preamble
		var meta any

		preamble, meta, err = r.Next()

		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		if !errors.Is(err, io.EOF) {
			if preamble != nil {
				if err = f(preamble, meta); err != nil {
					return err
				}
			} else {
				return err
			}
		} else {
			return nil
		}
	}
	return nil
}
