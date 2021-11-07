// The pdf package provide tools to parse, modify, create or fix pdf files
package pdf

import (
    "fmt"
    "os"
    "io"
    "bytes"

    "sort"
    "strconv"
    "strings"
)

const (
    INPUT_BUFFER_SIZE = 1024*1024*4     // should be bigger than the max token size
    XREF_ENTRY_SIZE = 20                // by PDF specs
    DEFAULT_DICTIONARY_SIZE = 8         // by default a small size
    _DEFAULT_STREAM_SIZE = 16
    _STARTXREF_SIZE = 512               // may need to be much larger
)

type PdfFile struct {

    Version     string
    Header      string

    // from body
    Objects     []*PdfObject             // in use objects in file offset order

    // from XREF table, updated from body
    ObjById     map[int64]*PdfObject     // object by ID, with start & stop offsets

    // from trailer
    Trailer     pdfDictionary            // unmodified, followed by the extracted values:
    Size        int64                    // always available
    Catalog     pdfReference             // always available, also know as Root
    Encrypt     pdfReference             // reference ID = 0 if not available
    Info        pdfReference             // reference ID = 0 if not available
    Id          *PdfArray                // nil if not available
}

type PdfObject   struct {                 // sortable by start offset
    id, gen     int64
    start, stop int64
    value       interface{}
}

type objBoundaries [](*PdfObject)
func (ob objBoundaries)Len() int {
    return len(ob)
}

func (ob objBoundaries)Less(i, j int) bool {
    return ob[i].start < ob[j].start
}

func (ob objBoundaries)Swap(i, j int) {
    ob[i], ob[j] = ob[j], ob[i]
}

// pdf type mapping to go types
type pdfBool        bool
type pdfNumber      float64
type pdfString      string
type pdfHexString   string
type pdfName        string

type pdfDictionary  struct {
    keys       []string     // (file) ordered list of map keys
    data        map[string]interface{}
}

type pdfStream      struct {
    extent      pdfDictionary
    data        []byte
}

type PdfArray       struct {
    data        []interface{}
}

type pdfNull        struct { }

type pdfReference   struct {
    id, gen     int64
}

type fileInput struct {
    fd          *os.File

    size        int64   // total file size
    bStart      int64   // current buffer file offset
    stopAt      int64   // boundary not to cross when refilling buffer

    buffer      []byte  // current read buffer
    offset      int     // offset in current read buffer

    // offset is always pointing after the content of bytes/token
    bytes       []byte  // requested n binary bytes (not lines)
    token       string  // current token in buffer
    tokFilePos  int64   // current token position in file

    // savedToken and savedOffset are used to restart with the
    // previous token.
    savedToken  string  // "" indicate no saved token
    savedOffset int     // -1 indicate no saved token
    savedPos    int64   // valid only if saved token is not ""

    verbose     bool    // print warnings and other information while parsing
    fix         bool    // try to recover from wrong PDF syntax
}

func (fi *fileInput) parseErrorf( format string, a ...interface{} ) error {
    f := fmt.Sprintf( "Parse error around offset 0x%x: %s", fi.bStart + int64(fi.offset), format )
    return fmt.Errorf( f, a... )
}

func (fi *fileInput) printParseInfo( format string, a ...interface{} ) {
    fmt.Fprintf( os.Stderr, "Around offset 0x%x: ", fi.bStart + int64(fi.offset) )
    fmt.Fprintf( os.Stderr, format, a... )
}

func (fi *fileInput)refill() error {

    remaining := len(fi.buffer) - fi.offset
    if remaining > 0 {  // refilling before the end because a token crosses the buffer boundary
        copy( fi.buffer[0:], fi.buffer[fi.offset:] )  // keep the last bytes in the next buffer
//        fmt.Printf( "refill buffer, keeping last %d bytes: '%s'\n", remaining, fi.buffer[0:remaining] )
    }
    // limits refill to fi.stopAt: maxSize = min( len(fi.buffer), fi.stopAt-(fi.bstart+len(fi.buffer)) )
    end := fi.stopAt - fi.bStart - int64(fi.offset)
    if end > int64(len(fi.buffer)) { end = int64(len(fi.buffer)) } // will need another refill later
/*
    fmt.Printf( "refill: buffer len 0x%x offset 0x%x remaining 0x%x end 0x%x file start 0x%x stop 0x%x size 0x%x\n",
                len(fi.buffer), fi.offset, remaining, end, fi.bStart, fi.stopAt, fi.size )
*/
    var n int
    var err error
    if end > int64(remaining) {                 // something to read
        tmpBuf := fi.buffer[remaining:end]      // keep unread fi.buffer data ptr
        n, err = fi.fd.Read( tmpBuf )
        if err != nil {
            if err != io.EOF { return err }
        } // n == 0 if io.EOF
    } else {
        n = 0
        err = io.EOF
    }
    fi.buffer = fi.buffer[:remaining+n]         // update fi.buffer len
    fi.bStart += int64(fi.offset)
    fi.offset = 0
    return err                                  // either nil or io.EOF
}

// Fills up to size bytes in buffer with bytes read from the file location fl
func (fi *fileInput)fillBuffer( size, fl int64 ) error {

    if size == 0 { return nil }

    _, err := fi.fd.Seek( fl, os.SEEK_SET )
    if err != nil { return err }
    fi.bStart = fl                // buffer start position in file
    fi.stopAt = fl + size         // set limits next refill if needed

    if size > INPUT_BUFFER_SIZE { // will require a refill during parsing
        size = INPUT_BUFFER_SIZE
    }
    tmpBuf := fi.buffer[0:size]
    n, err := fi.fd.Read( tmpBuf )
    if err != nil && err != io.EOF { return err }

    fi.buffer = fi.buffer[0:n]  // use buffer from 0 to n read bytes
    fi.offset = 0
    return nil
}

// skip to new offset relative to the beginning of the file, within current range
func (fi *fileInput) skipInBufferTo( offset int64 ) {
    if offset > fi.stopAt { panic( "skipping outside range\n" ) }
    if offset - fi.bStart >= int64(len( fi.buffer )) {
        fi.fillBuffer( fi.stopAt - offset, offset )
    } else {
        fi.offset = int(offset - fi.bStart)
    }
}

// get end of object offset to prevent reading beyond that if n is incorrect
func (fi *fileInput)readNBytes( n int64 ) (res int64) {
    if n == 0 {  return 0 }

    // create new byte slice since requested length may be
    // larger than a single fi buffer
    fi.bytes = make( []byte, n )

    bytes := fi.bytes
    for {   // possibly across multiple buffers
        end := int64( len(fi.buffer) )
        remaining := end - int64( fi.offset )
//        fmt.Printf( "requested %d, end %d, remaining %d, done %d\n", n, end, remaining, res )
        if n - res <= remaining {
            copy( bytes, fi.buffer[ fi.offset : fi.offset + int(n - res) ] )
            fi.offset += int(n - res)
            res = n
            return res
        }
        res += remaining
        copy( bytes, fi.buffer[ fi.offset : ] )
        bytes = fi.bytes[ res : ]
        fi.offset = int(end)
        err := fi.refill()
        if err != nil { return res } // truncated
    }
}

func (fi *fileInput)readUpToEndstream( ) ( int64, error ) {
    fi.bytes = make( []byte, 0, _DEFAULT_STREAM_SIZE )
    start := fi.getFilePos()
    for {   // possibly across multiple buffers
        end := bytes.Index( fi.buffer[fi.offset:], []byte("endstream") )
        if end != -1 { // found, exit
            end += fi.offset
            fi.bytes = append( fi.bytes, fi.buffer[fi.offset:end]... )
            fi.offset = end + len("endstream")
            if fi.verbose {
                fi.printParseInfo( "Found 'endstream' after %d bytes\n",
                                   fi.getFilePos() - start - int64(len("endstream")) )
            }
            return int64( len( fi.bytes ) ), nil
        }
        fi.bytes = append( fi.bytes, fi.buffer[fi.offset:]... ) 
        fi.offset = len(fi.buffer)
        err := fi.refill()          // search is limited to fi.stopAt
        if err != nil {
            return 0, fi.parseErrorf( "No 'endstream' within stream object: %v", err ) 
        }
    }
}

func (fi *fileInput)skipCurrentLF( ) ( bool, error ) {
    for {
        if fi.offset < len( fi.buffer ) {
            if fi.buffer[fi.offset] == '\n' {
                fi.offset ++
                return true, nil
            }
            return false, nil
        }
        err := fi.refill( )
        if err != nil { return false, fi.parseErrorf( "Premature EOF (expecting LF)\n" ) }
    }
}

func (fi *fileInput)skipCurrentEOL( crLf bool ) ( bool, error ) {
    for {
        if fi.offset < len( fi.buffer ) {
            c := fi.buffer[fi.offset]
            switch c {
            case '\r':
                if crLf { // must be followed by \n'
                    fi.offset ++
                    return fi.skipCurrentLF()
                }
                return true, nil
            case '\n':
                fi.offset ++
                return true, nil
            default:
                return false, nil
            }
        }
        err := fi.refill( )
        if err != nil { return false, fi.parseErrorf( "Premature EOF (expecting EOL)\n" ) }
    }
}

// skip multiple spaces and commments (%...\n)
func (fi *fileInput)skipSpaces( noComment bool ) {
    end := len( fi.buffer )
    inComment := false
    for {   // possbly across multiple buffers
        i := fi.offset
        for ; i < end; i ++ {
            switch fi.buffer[i] {
            case  '\n', '\r':
                inComment = false
            case ' ', '\t', '\f' :             // keep moving
            case '%':
                if noComment {
                    fi.offset = i              // stop right here
                    return
                }
                inComment = true           // in comment
            default:
                if inComment { continue }      // if comment skip
                fi.offset = i                  // else stop right here
                return
            }
        }
        fi.offset = i            // reached the end of current buffer
        err := fi.refill()       // get the next buffer, and continue
        if err != nil { return } // End of block, stop
        end = len( fi.buffer )
    }
}

func (fi *fileInput)eofComment( ) bool {
//    fmt.Printf( "EOF Comment: offset 0x%x, len 0x%x: %s\n", fi.offset, len(fi.buffer), string(fi.buffer[fi.offset:]) )
    if fi.offset + 4 >= len(fi.buffer) {
        err := fi.refill()
        if err != nil { return false } // end of block without EOF comment
    }
    i := fi.offset
    if fi.buffer[ i ] != '%' || fi.buffer[ i+1 ] != '%' || fi.buffer[ i+2 ] != 'E' ||
       fi.buffer[ i+3 ] != 'O' || fi.buffer[ i+4 ] != 'F' {
        return false
    }
    return true
}

func (fi *fileInput)getFilePos( ) int64 {
    return fi.bStart + int64(fi.offset)
}

// tokenize the buffer, moves offset to after space(s) following token
func (fi *fileInput)nextToken( ) {
    var sb strings.Builder

    fi.skipSpaces( false )     // skip spaces and comments
    if fi.savedOffset != -1 {  // update saved offset after skipping spaces
        fi.savedOffset = fi.offset  // as the buffer may have new content
    }

    fi.tokFilePos = fi.getFilePos() // next token starts here
    if fi.tokFilePos >= fi.stopAt {
        fi.token = ""
        return
    }
    end := len( fi.buffer )
    i := fi.offset

    previous := byte(0)
    for {   // token possibly across more than one buffer
        for ; i < end; i++ {
            b := fi.buffer[ i ]

            switch b {
            default:   //  b is not '<' and not '>'
                if sb.Len() == 1 && ( previous == '<' || previous == '>' ) {
                    fi.token = sb.String() // token is '<'or '>' alone
                    fi.offset = i       // do not consume following char
                    return
                }

            case '<', '>' :
                if sb.Len() > 1 {       // something before '<' or '>' is a first token
                    fi.token = sb.String()
                    fi.offset = i       // do not consume following '<' or '>'
                    return
                }
                if previous == '<' {    // 1 previous char
                    fi.token = "<<"     // token is '<<'
                    fi.offset = i + 1   // consume second '<'
                    return
                } else if previous == '>' {
                    fi.token = ">>"     // token is '>>'
                    fi.offset = i + 1   // consume second '>'
                    return
                }                       // else 0 previous char keep going at least one round

            case '/':
                if sb.Len() > 0 {       // not starting a new token
                    fi.token = string(b)
                    fi.offset = i       // not consumed
                    return
                }                       // else must be the start of a name

            case '[', ']', '(':       // '[', ']' or '(' are seen as separate
                if sb.Len() == 0 {    // tokens when starting a new token
                    fi.token = string(b)
                    fi.offset = i + 1 // consumed
                    return
                }
                fallthrough           // else seen as ending token
            // ends at first space, '[' or ']'
            case 0x0d, 0x0a, 0x00, 0x09, 0x0c, 0x20, '%' :
                fi.offset = i
                fi.token = sb.String()
                return
            }
            previous = b
            sb.WriteByte( b )
        }
        // token does not seem to end in current buffer.
        // make sure the begining of token is still in buffer after refill
        // so that it can be retrieved after restoring a saved token.
        // refill takes care of moving data from fi.offset to buffer end
        // to the head of the new buffer before filling the rest of the
        // buffer with new data.
        if err := fi.refill( ); err != nil {
            fi.offset = i
            fi.token = sb.String()  // premature end of token
            return
        }
        i = sb.Len()                // resume token where we stopped
        end = len( fi.buffer )
        if fi.savedOffset != -1 {   // update saved offset after refill
//fmt.Printf( "Fixing saved offset from 0x%x to 0X0\n", fi.savedOffset )
            fi.savedOffset = 0      // to point to the beginning of new token
        }
    }
 }

// save the current token, before looking ahead to the next token
// works only for 1 token lookahead
func (fi *fileInput)saveCurrentToken() {
//    fmt.Printf( "save current token '%s' offset 0x%x\n", fi.token, fi.offset )
    fi.savedToken = fi.token
    fi.savedPos = fi.tokFilePos
    fi.savedOffset = fi.offset  // points to space ahead of next token (if any)
}

// restore the saved token, after looking ahead to the next token
func (fi *fileInput)restoreCurrentToken() {
    fi.token = fi.savedToken
    fi.tokFilePos = fi.savedPos
    fi.offset = fi.savedOffset   // may have been updated by nextToken
    fi.savedToken = ""
    fi.savedOffset = -1
//    fmt.Printf( "restore current token '%s' offset 0x%x\n", fi.token, fi.offset )
}

func checkObjType( fi *fileInput, expected string ) error {
    if fi.token != expected {
        return fi.parseErrorf( "Not an object: %s\n", fi.token )
    }
    fi.nextToken()
    return nil
}

func (fi *fileInput)getByte( ) byte {

    if fi.offset < len( fi.buffer ) {
        c := fi.buffer[ fi.offset ]
        fi.offset++
        return c
    }
    err := fi.refill()
    if err != nil { return 0x0a }
    return fi.getByte()
}

// Undo getByte by decrementing fi.offset
// It is only guaranteed to work once, and only after calling getByte
// since the same buffer was used to return the byte to push back.
// (at worst getByte did a refill just before returning the first byte
// and fi.offset is 1 when returning from getByte)
func (fi *fileInput)ungetByte( ) {
    fi.offset --
}

// consumes from 0 byte to 3 bytes
func writeEscapeSeq( fi *fileInput, sb *strings.Builder ) {

    switch c := fi.getByte( ); c {
    case 'n', 'r', 't', 'b', 'f', '(', ')', '\\', '\n', '\r':
        (*sb).WriteByte( '\\' )
        (*sb).WriteByte( c )

    default:    // octal number
        if c < 0x30 || c > 0x37 {
            fi.ungetByte()  // ignore '\' not followed by valid characters
            return
        }
        (*sb).WriteByte( '\\' )
        (*sb).WriteByte( c )
        c = fi.getByte( )
        if c < 0x30 || c > 0x37 {
            fi.ungetByte()
            return
        }
        (*sb).WriteByte( c )
        c = fi.getByte()
        if c < 0x30 || c > 0x37 {
            fi.ungetByte()
            return
        }
        (*sb).WriteByte( c )
    }
}

func getLiteralString( fi *fileInput ) ( pdfString, error ) {
    var sb strings.Builder
//    fmt.Printf("getLiteralString\n" )
    openCount := 1
    for {   // possibly across multiple buffers
        end := len(fi.buffer)
        for i := fi.offset; i < end; i++ {
            switch c := fi.buffer[i]; c {
            case '(':
//                fmt.Printf("Seen '(', openCount=%d\n", openCount )
                sb.WriteByte( c )
                openCount ++
            case ')':
//                fmt.Printf("Seen ')', openCount=%d\n", openCount )
                openCount --
                if openCount == 0 {     // end of string
//                    fmt.Printf( "End of literal string, next=0x%x\n", i+1 )
                    fi.offset = i + 1
                    fi.nextToken()
                    return pdfString( sb.String() ), nil
                }
                sb.WriteByte( c )
            case '\\':
//                fmt.Printf( "Entering escape sequence with end %x, i %x\n", end, i )
                fi.offset = i + 1
                writeEscapeSeq( fi, &sb )
                end = len(fi.buffer)   // buffer & offset may have changed
                i = fi.offset -1       // i++ at the end of the loop
//                fmt.Printf( "Exiting escape sequence with end %x, i %x\n", end, i )
            default:
                sb.WriteByte( c )
            }
        }
        err := fi.refill()
        if err != nil {
            return pdfString(""), fi.parseErrorf( "End of file within a literal string (%s)\n", sb.String() )
        }
    }
}

func checkExpectedEndStream( fi *fileInput, l, stop int64 ) int64 {
    // Step 1: Check if l is beyond the objects or beyond file boundary, then if it is beyond the
    // object boundary (if known) and if it is, then return not found (-1)
    // Step 2: lookup the few bytes around the stream end (past l), and see if 'endstream' can be found.
    // if it is return the actual length from current fi.offset and 'endstream'

    expectedEndPos := fi.bStart + int64(fi.offset) + l
    if expectedEndPos > fi.stopAt || expectedEndPos > fi.size { return -1 }
    if stop != -1 && expectedEndPos > stop { return -1 }

    // try to read the 15 bytes following expectedEnd ("endstream\r\n")
    var localBuffer []byte
    var end int
    expectedEndOffset := int64(fi.offset) + l
    if expectedEndOffset + 15 < int64(len(fi.buffer)) { // can use current buffer
        localBuffer = fi.buffer[expectedEndOffset:expectedEndOffset+15]
    } else {    // use a small local buffer and read 256 bytes
        localBuffer = make( []byte, 256 )
        pos, _ := fi.fd.Seek( 0, os.SEEK_CUR )
        fi.fd.Seek( expectedEndPos, os.SEEK_SET )
        n, err := fi.fd.Read( localBuffer )
        fi.fd.Seek( pos, os.SEEK_SET )
        if err != nil { return -1 }
        localBuffer = localBuffer[:n]
    }
    end = bytes.Index( localBuffer, []byte("endstream") )
    if end == -1 { return -1 }  // no 'endtream' in sight

    if end > 0 && end <= 2 {
        switch end {
        case 2:
            if localBuffer[end-1] != 0x0a { break }
            if localBuffer[end-2] != 0x0d { break }
            end = 0 // ignore EOL before endstream

        case 1:
            if localBuffer[end-1] != 0x0a && localBuffer[end-1] != 0x0d { break }
            end = 0 // ignore EOL before endstream
        }
    }

    return l + int64(end)
}

func getStreamBytes( fi *fileInput, l, stop int64 ) ( []byte, error ) {
    if skip, err := fi.skipCurrentEOL( true ); ! skip || err != nil {
        return nil, fi.parseErrorf( "Stream keyword without following EOL\n" )
    }

    // if 'endstream' is not found close to l, lookup for endstream starting from the beginning
    // of the stream lookup for endstream. If found, stop here and calculate the actual length
    // else stop when reaching the end of the object (if known) or end of file and return error
    actual := checkExpectedEndStream( fi, l, stop ) // get actual length
    if actual == -1 {                               // no 'endstream' at the expected end of stream
        if ! fi.fix {
            return nil, fi.parseErrorf( "No 'endstream' at stream end (length %d)\n", l )
        }
        if fi.verbose {
            fi.printParseInfo( "No 'endstream' at stream end (length %d): searching in stream data and beyond\n", l )
        }
        var err error
        actual, err = fi.readUpToEndstream( )
        if err != nil {
            return nil, err
        }
    } else {                                        // we know now the actual length
        if l != actual {
            if ! fi.fix {
                return nil, fi.parseErrorf( "Stream length %d does not match actual length: %d\n", l, actual )
            }
            if fi.verbose {
                fi.printParseInfo( "Stream length %d does not match actual length: %d\n", l, actual )
            }
        }
        if l = fi.readNBytes( actual ); l < actual {    // something is wrong
            panic( fmt.Sprintf( "Stream object with wrong length %d: remaining %d\n", actual, l ) )
        }
        fi.nextToken()                                  // eat the following 'endstream'
        if fi.token != "endstream" {                    // something is very wrong
            panic( fmt.Sprintf( "Stream object without 'endstream' or with wrong length %d: %s\n", l, fi.token ) )
        }
    }
    fi.nextToken()  // get ready for the next token
//    fmt.Printf( "getStreamBytes: followed by token '%s'\n", fi.token )
    return fi.bytes, nil
}

func getDictionaryOrStream( fi *fileInput, stop int64 ) ( interface{}, error ) {
//    fmt.Printf( "getDictionaryOrStream, offset: 0x%x, stop: 0x%0x\n", fi.offset, stop )
    k := make( []string, 0, DEFAULT_DICTIONARY_SIZE )
    m := make( map[string]interface{} )
//    fmt.Printf( "getDictionaryOrStream: before: '%s'\n", fi.token )
    fi.nextToken() // skip "<<"
//    fmt.Printf( "getDictionaryOrStream: token: '%s'\n", fi.token )
    for {
        if fi.token == ">>" {
            fi.nextToken( )
            if fi.token == "stream" { // stream must be followed by EOL
                n, ok := m["Length"]; if ! ok {
                    return nil, fi.parseErrorf(  "Stream object without Length in dictionary: %v\n", m )
                }
                l := n.(pdfNumber)
                stream, err := getStreamBytes( fi, int64(l), stop )
                if err != nil {
                    return nil, fmt.Errorf(  "Stream object error: %v", err )
                }
                if len(stream) != int(l) { // only possible if fi.fix is true, otherwise an error was returned
                    fi.printParseInfo( "Setting stream extent to length %d (previously %d)\n", len(stream), int(l))
                    m["Length"] = pdfNumber(len(stream))    // note that actual stream checking might change the length
                }
                return pdfStream{ extent: pdfDictionary{ k, m }, data: stream }, nil
            }
            return pdfDictionary{ k, m }, nil
        }
        // expect a name first
        if fi.token[0] != '/' {
            return nil, fi.parseErrorf( "Dictionary entry is not a name: %s\n", fi.token )
        }
        pName, err := getName( fi )
        if err != nil {
            return nil, fmt.Errorf( "Dictionary key is invalid: %v", err )
        }
        k = append( k, string(pName) )
        // then an object
        m[string(pName)], err = getObjectDef( fi, stop )
        if err != nil {
            return nil, fmt.Errorf( "Dictionary value is invalid: %v", err )
        }
    }
}

func getHexString( fi *fileInput ) ( pdfHexString, error ) {
    var hsb strings.Builder
    var nibble byte
    // <xx..xx>
//    fmt.Printf("HexString: %s\n", fi.token )
    n := 0  // used to find the nibble parity
mainLoop:
    for {   // possibly across more than one buffer
        end := len(fi.buffer)
        for i := fi.offset; i < end; i++ {
            switch c := fi.buffer[i]; c {
            case 0x0d, 0x0a, 0x00, 0x09, 0x0c, 0x20: // any skip white space
            case '>':           // indicate end of string
                if n & 1 == 1 { // missing last number, assumed to be 0
                    hsb.WriteByte( nibble << 4 )
                }
                fi.offset = i+1 // comsume '>' & exit
                break mainLoop
            default:
                if n & 1 == 0 { // first nibble
                    nibble = makeNibbleFromHexChar( c )
                    if nibble == 0xff {
                        return pdfHexString(""), fi.parseErrorf( "Not an hexadecimal digit: 0x%x\n", c )
                    }
                } else {        // second nibble
                    lower := makeNibbleFromHexChar( c )
                    if nibble == 0xff {
                        return pdfHexString(""), fi.parseErrorf( "Not an hexadecimal digit: 0x%x\n", c )
                    }
                    hsb.WriteByte( nibble << 4 + lower )
                }
                n ++
            }
        }
        fi.offset = end
        err := fi.refill()
        if err != nil {
            return pdfHexString(""), fi.parseErrorf( "End of file within a hex string (%s)\n", hsb.String() )
        }
    }
    fi.nextToken()
    return pdfHexString( hsb.String() ), nil
}

func makeNibbleFromHexChar( c byte ) byte {
    if c < 0x30 {
        return 0xff         // invalid nibble
    }
    if c <= 0x39 {
        return c - 0x30
    }
    c &^= 0x20              // a => A, ...
    if c < 0x41 || c > 0x46 {
        return 0xff         // invalid nibble
    }
    return c - 0x41 + 10    // A, B, C, D, E, F => 10, 11, 12, 13, 14, 15
}

func getName( fi *fileInput ) ( pdfName, error ) {
    var name strings.Builder
    token := fi.token
//    fmt.Printf( "name token: '%s'\n", token )
    for i := 1; i < len( token ); { // skip '/'
        switch token[i] {
        case '#':  // check for correctness but do not decode encoded name
            if i + 2 >= len(token) {
                return pdfName(""), fi.parseErrorf( "Incomplete # escape in name\n" )
            }
            name.WriteByte( '#' )
            c := makeNibbleFromHexChar( token[i+1] )
            if c == 0xff {
                return pdfName(""), fi.parseErrorf( "Not an hexadecimal digit: 0x%x\n", token[i+1] )
            }
            name.WriteByte( token[i+1] )
            c <<= 4
            c += makeNibbleFromHexChar( token[i+2] )
            if c == 0xff {
                return pdfName(""), fi.parseErrorf( "Not an hexadecimal digit: 0x%x\n", token[i+2] )
            }
            name.WriteByte( token[i+2] )
            i += 3
        default:
            name.WriteByte( token[i] )
            i ++
        }
    }
    fi.nextToken()
    return pdfName( name.String() ), nil
}

func getArray( fi *fileInput, stop int64 ) ( PdfArray, error ) {
    array := make( []interface{}, 0, 4 )   // expect mostly small arrays
    fi.nextToken()
    for {
        if fi.token == "]" {
            fi.nextToken()
//            fmt.Printf( "Array: %v\n", array )
            return PdfArray { array }, nil
        }
//        fmt.Printf( "Array: token '%s'\n", fi.token )
        val, err := getObjectDef( fi, stop )
        if err != nil {
            return PdfArray{nil}, fi.parseErrorf( "Array element is invalid: %v", err )
        }
        array = append( array, val )
        if stop != -1 && stop <= fi.getFilePos() {
            return PdfArray{nil}, fi.parseErrorf( "Array reached the end of object before ending (0x%x)\n", stop )
        }
    }
}

func getNumber( fi *fileInput ) ( pdfNumber, error ) {
    rn, err := strconv.ParseFloat( fi.token, 64 )
    if err != nil {
        return pdfNumber(0), fi.parseErrorf( "Invalid floating point number %s\n", fi.token )
    }
    fi.nextToken()
    return pdfNumber( rn ), nil
}

func getPositiveInteger( token string ) (int64, bool) {
    if ( token[0] < '0' || token[0] > '9' ) && token[0] != '+' {
//        fmt.Printf( "getPositiveInteger: illegal char 0x%x (%s)\n", token[0], token )
        return 0, false
    }
    in, err := strconv.ParseInt( token, 10, 32 )
    if err != nil {
//        fmt.Printf( "getPositiveInteger: error %v\n", err )
        return 0, false
    }
    return in, true
}

func getNumberOrObjRef( fi *fileInput ) ( interface{}, error ) {
    // either single number (+/-n.m) or object reference (n g R)
    // fi.token is the first number, it may be followed by an 
    // another number and then byt 'R'. If yes, the result is
    // an object reference. If not, the result is the first
    // number and the 2 following objects are stored in a
    // lookahead buffer.
//    fmt.Printf("getNumberOrObjRef: token: '%s' @0x%x\n", fi.token, fi.offset )
    id, ok := getPositiveInteger( fi.token )
    if ! ok { // not an integer, may be a real number
//        fmt.Printf( "getNumberOrObjRef returns real number\n" )
        return getNumber( fi )
    }

    fi.nextToken()
//    fmt.Printf("getNumberOrObjRef: next token: '%s'\n", fi.token )
    g, ok := getPositiveInteger( fi.token )
    if ok {
        fi.saveCurrentToken()
        fi.nextToken()
//        fmt.Printf("getNumberOrObjRef: next next token: '%s'\n", fi.token )
        if fi.token == "R" {
            fi.nextToken()
//            fmt.Printf( "getNumberOrObjRef returns indirect reference %d %d with next token='%s'\n",
//                         id, g, fi.token )
            return pdfReference{ id, g }, nil
        }
        fi.restoreCurrentToken()
    }
//    fmt.Printf( "getNumberOrObjRef returns single integer number %d with next token='%s'\n", id, fi.token )
    return pdfNumber( float64( id ) ), nil
}

func getIndirectObjectDef( fi *fileInput ) ( int64, int64, error ) {

    // assumes first token is ready
    id, err := strconv.ParseInt( fi.token, 10, 64 )
    if err != nil {
        return 0, 0, fmt.Errorf( "Invalid indirect object id: %s : %v", fi.token, err )
    }
    fi.nextToken()
    gen, err := strconv.ParseInt( fi.token, 10, 64 )
    if err != nil {
        return 0, 0, fmt.Errorf( "Invalid indirect object %d generation: %s : %v", id, fi.token, err )
    }
    fi.nextToken()
    if fi.token != "obj" {
        return 0, 0, fi.parseErrorf( "invalid object %d generation %d syntax %s\n", id, gen, fi.token )
    }
    fi.nextToken()
    return id, gen, nil
}

func getObjectDef( fi *fileInput, stop int64 ) ( interface{}, error ) {
    var result interface{}
    var err error = nil
    if stop != -1 && stop <= fi.getFilePos() {
//        panic( fmt.Sprintf( "Reached the end of parent object before ending (stop 0x%x, current 0x%x)\n", stop, fi.getFilePos() ) )
        return nil, fi.parseErrorf( "Reached the end of parent object before ending (0x%x)\n", stop )
    }
//    fmt.Printf( "getObjectDef: token: '%s'\n", fi.token )
    switch fi.token[0] {
    case 't':           // boolean
        err = checkObjType( fi, "true" )
        result = pdfBool( true )
    case 'f':           // boolean
        err = checkObjType( fi, "false" )
        result = pdfBool( false )
    case '(':           // string
        result, err = getLiteralString( fi )
    case '<':           // hex string or dictionary
        if fi.token == "<<" {
            result, err = getDictionaryOrStream( fi, stop )
        } else {
            result, err = getHexString( fi )
        }
    case '/':           // name (in same token /name)
        result, err = getName( fi )
    case '[':           // array
        result, err = getArray( fi, stop )
    case 's':           // stream
        if err = checkObjType( fi, "stream" ); err != nil {
            err = fi.parseErrorf( "stream without dictionary and without length\n" )
        }
    case 'n':           // null
        err = checkObjType( fi, "null" )
        result = pdfNull{ }
    case '-', '.' :     // negative number, either integer or real, or positive real number
        result, err = getNumber( fi )
    case '+':           // may be start of a positive number or integer in an object reference?
        result, err = getNumberOrObjRef( fi )
    default:            // may be start of a number or an object reference?
        if fi.token[0] < '0' || fi.token[0] > '9' {
            err = fi.parseErrorf( "Invalid token or number: %s\n", fi.token )
        } else {
            result, err = getNumberOrObjRef( fi )
        }
    }
    if err != nil { return nil, err }
    return result, nil
}

func printPdfObj( obj interface{}, indent string ) {
    switch obj := obj.(type) {
    case pdfBool:
        fmt.Printf("%s Bool %t\n", indent, obj )
    case pdfNumber:
        fmt.Printf("%s Number %g\n", indent, obj )
    case pdfString:
        fmt.Printf("%s String %s\n", indent, obj )
    case pdfHexString:
        fmt.Printf("%s Hex string 0x%x\n", indent, obj )
    case pdfName:
        fmt.Printf("%s Name %s\n", indent, obj )
    case pdfDictionary:
        fmt.Printf("%s Dictionary:\n", indent )
        for k, v := range( obj.data ) {
            fmt.Printf( "%s   %s: ", indent, k )
            printPdfObj( v, indent + "  " )
        }
    case pdfStream:
        fmt.Printf( "%s Stream extent:\n", indent )
        printPdfObj( obj.extent, indent + "  " )
        fmt.Printf( "%s Stream length: %d\n", indent, len( obj.data ) )
    case PdfArray:
        fmt.Printf("%s Array:\n", indent)
        for i, v := range( obj.data ) {
            fmt.Printf("%s   %d: ", indent, i )
            printPdfObj( v, indent + "  " )
        }
    case pdfNull:
        fmt.Printf( "%s Null", indent )
    case pdfReference:
        fmt.Printf( "%s Indirect object reference: id %d generation %d\n",
                    indent, obj.id, obj.gen )
    }
}

func (pf *PdfFile) parseObjects( fi *fileInput, objStart, objEnd int64 ) error {

    if objEnd != 0 { // case of the main body, otherwise use the current buffer
        fi.fillBuffer( objEnd - objStart, objStart )
    }
//    fmt.Printf( "parseObjects: objStart: 0x%x, offset 0x%x, objEnd: 0x%x\n", fi.bStart, fi.offset, fi.stopAt )
    if fi.verbose {
        fi.printParseInfo( "Body starts\n" )
    }

    for {
        fi.nextToken()
// DEBUG
//fmt.Printf( "indirect object new token %s position 0x%x\n", fi.token, fi.tokFilePos )
//fmt.Printf( "new object buffer offset %d, end buffer 0x%x\n", fi.offset, len( fi.buffer ) )
// END DEBUG
//        fmt.Printf( "Parsing 1 indirect object, First token: %s\n", fi.token )
        // expect an indirect object definition: nnn mm obj\n
        if fi.token == "" || fi.token == "xref" { break }

        offset := fi.tokFilePos
        id, gen, err := getIndirectObjectDef( fi )
        if err != nil {
             return fmt.Errorf( "Indirect Object ID or generation: %v", err )
         }
//        fmt.Printf( "Indirect Object %d %d, offset: 0x%x\n", id, gen, offset )
        
        objDef := pf.ObjById[ id ]
        if objDef != nil && objDef.gen == gen {
//        fmt.Printf( "ObjById: %d %d, start 0x%x, end 0x%x\n",
//                    objDef.id, objDef.gen, objDef.start, objDef.stop )
            if objDef.start == -1 { objDef.start = offset }
            obj, err := getObjectDef( fi, objDef.stop )
            if err != nil {
                return fmt.Errorf( "Indirect Object %d %d has an invalid definition: %v", id, gen, err )
            }
            if fi.token != "endobj" {
                return fi.parseErrorf( "Indirect Object %d %d does not end with 'endobj': %s\n", id, gen, fi.token )
            }
            objDef.value = obj   // update object definition with its value
//            printPdfObj( obj, "" )
            if objDef.stop == -1 { objDef.stop = fi.tokFilePos }
            pf.Objects = append( pf.Objects, objDef ) // keep pointer to objects in file order

        } else {
            fi.skipInBufferTo( objDef.stop ) // skip object
        }
    }
    return nil
}

// func printObjBoundary( ob *PdfObject ) {
//     fmt.Printf( "obj ID %d, gen %d, offset 0x%x ends @0x%x\n", ob.id, ob.gen, ob.start, ob.stop )
// }

func (pf *PdfFile) parseXrefSubsection( fi *fileInput, maxObjPos, start, number int64 ) error {
    if fi.verbose {
        fi.printParseInfo( "XEF subsection [%d:%d] (included)\n", start, start + number -1 )
    }

    // start & eventually stop offset of each object after sorting by start offset
    boundaries := make( objBoundaries, number )
    badOffset := int64(-1)  // -1 if offsets look valid, the number of entries to invalidate if bad offsets
    end := len( fi.buffer ) - XREF_ENTRY_SIZE
    nInUse := int64(0)
    for i:= int64(0); i < number; i++ {
        if fi.offset > end {
//            fmt.Printf("End of buffer, refilling with fi.offset %d\n", fi.offset)
            fi.refill()     // preserves the first few bytes of the next entry
//            fmt.Printf("After refilling, fi.offset %d\n", fi.offset)
        }
        sepPos := fi.offset + 10
// uncomment to check for exact syntax checking
//        if fi.buffer[sepPos] != ' ' {
//            return fi.parseErrorf( "Incorrect offset separator 0x%x\n", fi.buffer[sepPos] )
//        }
        os := string(fi.buffer[fi.offset:sepPos])
        offset, ok := getPositiveInteger( os )
        if ! ok {
            return fi.parseErrorf( "Incorrect XREF offset %s\n", os )
        }

        gs := string(fi.buffer[sepPos+1:sepPos+6])
//        var gnumber int64
//        gnumber, ok = getPositiveInteger( gs )
        gen, ok := getPositiveInteger( gs )
        if ! ok {
            return fi.parseErrorf( "Incorrect XREF generation number %s\n", gs )
        }
// uncomment to check for exact syntax checking
//        if fi.buffer[sepPos+6] != ' ' {
//            return fi.parseErrorf( "Incorrect generation separator 0x%x\n", fi.buffer[sepPos+6] )
//        }

        var inUse bool
        switch fi.buffer[sepPos+7] {
        case 'n':  inUse = true
        case 'f':  inUse = false
        default:
            return fi.parseErrorf( "Incorrect XREF keyword 0x%x\n", fi.bytes[17] )
        }
// uncomment to chek for exact syntax checking
//        if fi.bytes[18] != ' ' {
//            if fi.bytes[18] != '\r' || fi.bytes[19] != '\n' {
//                return fi.parseErrorf( "invalid end of line 0x%x%x\n", fi.bytes[18], fi.bytes[19] )
//            }
//        } else {
//            if fi.bytes[19] != '\r' && fi.bytes[19] != '\n' {
//                return fi.parseErrorf( "invalid end of line 0x%x%x\n", fi.bytes[18], fi.bytes[19] )
//            }
//        }
        if inUse {  // free objects are just ignored
            if badOffset != -1 || offset > maxObjPos {
                if ! fi.fix {
                    return fi.parseErrorf( "Incorrect XREF object offset 0x%x (beyond object boundary 0x%x)\n",
                                           offset, maxObjPos )
                }
                if badOffset == -1 {
                    if fi.verbose {
                        fi.printParseInfo( "XREF object offset 0x%x beyond object range (max 0x%x)\n",
                                           offset, maxObjPos )
                    }
                    badOffset = nInUse // one bad offset invalids all previous and following offsets
                } 
                offset = -1 // invalid start offset
            }
            boundaries[nInUse] = &PdfObject{ id: start+i, gen: gen, start: offset, stop: -1 }
            nInUse ++
        }
        fi.offset += XREF_ENTRY_SIZE
    }

    if badOffset == -1 { // valid XREF entries, reorder list to calculate each object end
        sort.Sort( boundaries[:nInUse] ) // sort boundaries by incrementing start offset

        for i := int64(0); i < nInUse - 1; i++ {  // last object has unknown stop (-1)
            boundaries[i].stop = boundaries[i+1].start
//            printObjBoundary( boundaries[i] )
        }
    } else { // incorrect offsets starting at badOffset: invalidates the previous entries since they cannot be trusted
        for i := int64(0); i < badOffset - 1; i++ {
            boundaries[i].start = -1
        }
    }

    for i := int64(0); i < nInUse; i++ {
        id := boundaries[i].id
        if _, ok := pf.ObjById[id]; ! ok { // do not replace previously found objboundaries (they were updated)
            pf.ObjById[id] = boundaries[i]
        }
    }
    return nil
}

func (pf *PdfFile) parseXrefTable( fi *fileInput, xrefStart, xrefEnd int64 ) error {

    // estimate an upper bound for the xref table size. This includes the
    // trailer, which takes perhaps 2 or 3 XREF entries (40 to 60 bytes)
    size := (xrefEnd - xrefStart) / XREF_ENTRY_SIZE
    pf.ObjById = make( map[int64]*PdfObject, size )
    fi.fillBuffer( xrefEnd - xrefStart, xrefStart ) // now use the full buffer

//    fmt.Printf( "parseXrefTable file start: 0x%x, offset: 0x%x, end: 0x%x\n", fi.bStart, fi.offset, fi.stopAt )
    fi.nextToken()
    // cross-reference table is made of multiple xref sections
    // each xref section is made of 1 or more subsections
    // subsection: 1 line with 2 integers: start space number eol
    for {
        if fi.token != "xref" { return nil }
        fi.nextToken( )
        start, ok := getPositiveInteger( fi.token )
        if ! ok {
            return fi.parseErrorf( "Incorrect xref section: start %s\n", fi.token )
        }
        fi.nextToken( )
        number, ok := getPositiveInteger( fi.token )
        if ! ok {
            return fi.parseErrorf( "Incorrect xref section: number %s\n", fi.token )
        }
        fi.skipSpaces( false )     // skip spaces and comments
        if err := pf.parseXrefSubsection( fi, xrefStart, start, number ); err != nil {
            return fmt.Errorf( "Incorrect xref section: %v", err )
        }
        fi.nextToken()
    }
}

func (pf *PdfFile) getRefFromTrailer( dic pdfDictionary, key string ) *pdfReference {
    if v, ok := dic.data[ key ]; ok {
        if ref, ok := v.(pdfReference); ok {
            return &ref
        }
    }
    return nil
}

func (pf *PdfFile) parseTrailer( fi *fileInput ) ( int64, error ) {
//    fmt.Printf( "trailer file start: 0x%x, offset: 0x%x, end: 0x%x\n", fi.bStart, fi.offset, fi.stopAt )
    if fi.verbose {
        fi.printParseInfo( "Trailer starts\n" )
    }

    if fi.token != "trailer" {
        return 0, fi.parseErrorf( "No trailer found: %s\n", fi.token )
    }
    fi.nextToken( )
    if fi.token != "<<" {
        return 0, fi.parseErrorf( "Trailer does not have a dictionary: %s\n", fi.token )
    }
    dos, err := getDictionaryOrStream( fi, fi.stopAt )
    if err != nil {
        return 0, fmt.Errorf( "Trailer dictionary is invalid: %v", err )
    }
    dic, ok := dos.(pdfDictionary)
    if ! ok {
        return 0, fi.parseErrorf( "Trailer does not have a dictionary (unexpected stream)\n" )
    }
    pf.Trailer = dic
//    printPdfObj( dic, "  " )

    if v, ok := dic.data["Size"]; ok {
        pf.Size = int64(v.(pdfNumber))
    } else {
        return 0, fi.parseErrorf( "Trailer dictionary does not provide XREF size\n" )
    }
    if v := pf.getRefFromTrailer( dic, "Root" ); v != nil {
        pf.Catalog = *v
    } else {
        return 0, fi.parseErrorf( "Trailer dictionary does not provide root catalog\n" )
    }

    if v := pf.getRefFromTrailer( dic, "Encrypt" ); v != nil {
        pf.Encrypt = *v
    }

    if v := pf.getRefFromTrailer( dic, "Info" ); v != nil {
        pf.Info = *v
    }

    if pa, ok := dic.data[ "ID" ]; ok {
        if v, ok := pa.(PdfArray); ok {
            pf.Id = &v
            a := v.data
//            var Id0, Id1 pdfHexString
//            Id0, ok = a[0].(pdfHexString)
            _, ok = a[0].(pdfHexString)
            if ! ok { return 0, fi.parseErrorf( "ID #0 is not an hexString: %v\n", a[0] ) }
//            Id1, ok = a[1].(pdfHexString)
            _, ok = a[1].(pdfHexString)
            if ! ok { return 0, fi.parseErrorf( "ID #1 is not an hexString: %v\n", a[1] ) }
//            fmt.Printf( "IDs: <%x> <%x>\n", Id0, Id1 )
        }
    }

    if fi.token != "startxref" {
        return 0, fi.parseErrorf( "Trailer does not have a start xref: %s\n", fi.token )
    }
    fi.nextToken( )
//    var xrefOffset int64, 
//    xrefOffset, ok = getPositiveInteger( fi.token )
    _, ok = getPositiveInteger( fi.token )
    if ! ok {
        return 0, fi.parseErrorf( "Trailer does not have a valid start xref offset: %s\n", fi.token )
    }
//    fmt.Printf("Start XREF offset: %d (0x%x)\n", xrefOffset, xrefOffset )
    fi.skipSpaces( true )     // skip spaces but not comments
    if ! fi.eofComment( ) {
        return 0, fi.parseErrorf( "Trailer does not have a correct EOF\n" )
    }
    if pXref, ok := dic.data["Prev"]; ok {
        if v, ok := pXref.(pdfNumber); ok {
            return int64(v), nil
        }
    }
    return 0, nil    // not a possible XREF offset
}

func huntForXref( fi *fileInput ) ( int64, error ) {
    fi.offset = 0
    start, err := fi.fd.Seek( -_STARTXREF_SIZE, os.SEEK_END )
    if err != nil {
        return -1, fmt.Errorf("Empty file: %v", err )
    }
    fi.bStart = start      // to allow reporting the proper error location
    _, err = fi.fd.Read( fi.buffer )
    if err != nil {
        return -1, fi.parseErrorf("Unable to read file: %v", err )
    }
    // search for 'startxref'
    token := []byte("startxref")
    offset := bytes.LastIndex( fi.buffer, token )
    if -1 == offset {
        return -1, fi.parseErrorf("No 'startxref' in file\n" )
    }
    fi.offset = offset
    endSearch := int64(offset) // end of search in case of recovery below
    endToken := offset + len( token )

//    fmt.Printf( "offset %d, endToken %d char 0x%x \n", fi.offset, endToken, fi.buffer[ endToken ] )

    switch fi.buffer[ endToken] {
    case '\r' :
        if endToken >= _STARTXREF_SIZE {
            return -1, fi.parseErrorf("Missing startxref value\n" )
        }
        if fi.buffer[ endToken + 1 ] == '\n' {
            endToken += 2
        } else {
            endToken += 1
        }
    case '\n' :
        endToken += 1
    }

    fi.offset = endToken
    endLine := bytes.IndexAny( fi.buffer[endToken:], "\r\n" )
    if endLine == -1 {
        return -1, fi.parseErrorf("Missing startxref value\n" )
    }
    endLine += endToken
//    fmt.Printf( "endLine: %d\n", endLine )
    startXref, err := strconv.ParseInt( string(fi.buffer[ endToken : endLine ]), 10, 64 )
    if err != nil {
        return -1, fmt.Errorf("Invalid startxref value: %v", err )
    }
    if startXref > fi.size {
        if ! fi.fix {
            return -1, fi.parseErrorf("Startxref value is beyond end of file (0x%x)\n", startXref )
        }
        if fi.verbose {
            fi.printParseInfo( "Startxref value is beyond end of file (0x%x) searching for XREF\n", startXref )
        }
        back := int64(_STARTXREF_SIZE) // go back the last segment
        for {
            startXref = int64(bytes.LastIndex( fi.buffer[:endSearch], []byte("xref") ))
            if startXref != -1 {
                startXref += fi.bStart
                if fi.verbose {
                    fmt.Fprintf( os.Stderr, "XREF found at offset 0x%x\n", startXref )
                }
                break
            }
//            fmt.Printf( "Hunting for XREF: not in last buffer\n" )
            endSearch = int64(_STARTXREF_SIZE) + 5   // one segment + enough for [x]ref
            back += int64(_STARTXREF_SIZE) // back one more segment
            start, err := fi.fd.Seek( - back, os.SEEK_END )
            if err != nil {
                return -1, fmt.Errorf( "No XREF section: %v", err )
            }
            fi.bStart = start      // to allow reporting the proper error location
            _, err = fi.fd.Read( fi.buffer[:endSearch] )
            if err != nil {
                return -1, fi.parseErrorf("Unable to read file: %v", err )
            }
        }
    }
//    fmt.Printf( "startXref: %d\n", startXref )
    return startXref, nil
}

func (pf *PdfFile) parseHeader( fi *fileInput ) error {
    var err error
    pf.Header, err = fi.readFirstLine()
    if err != nil {
        return err
    }
    minor := pf.Header[7] - 0x30
    pf.Version = fmt.Sprintf( "1.%d", minor )
    return nil
}

func skipToEOL( buffer []byte, offset int ) int {
    end := len( buffer )
Loop:
    for ; offset < end; offset ++ {
        switch buffer[ offset ] {
        case 0x0d:
            if offset + 1 == end { break Loop }
            next := buffer[ offset + 1 ]   // peek next byte
            if next != 0x0a { break Loop }
            return offset + 2              // skip terminating LF
        case 0x0a:
            break Loop
        }
    }
    return offset + 1
}

func (fi *fileInput)readFirstLine( ) ( string, error ) {
    err := fi.fillBuffer( 512, 0 )  // only first 512 bytes
    if err != nil {
        return "", fi.parseErrorf( "Empty file: %v", err )
    }
    end := len(fi.buffer)
   // expects %PDF-1.?\n
    if end < 9 || '%' != fi.buffer[0] ||
       'P' != fi.buffer[1] || 'D' != fi.buffer[2] ||
       'F' != fi.buffer[3] || '-' != fi.buffer[4] ||
       '1' != fi.buffer[5] || '.' != fi.buffer[6] {
        return "", fi.parseErrorf( "Not a PDF document\n" )
    }
    fi.offset = 8
    switch fi.buffer[ 8 ] {
    case 0x0d:
        if end == 9 { break }
        next := fi.buffer[ 9 ]          // peek next byte
        if next == 0x0a { fi.offset++ } // skip terminating LF
    case 0x0a:  // as expected, do nothing
    default:
        return "", fi.parseErrorf( "Invalid file header\n" )
    }
    // skip the recommended binary comment following the PDF header
    // if any, and all following spaces. The next byte in the buffer
    // is the beginning of the objects in the file body.
    fi.offset++
    i := fi.offset
    if i >= end - 5 {  // required final eofComment is also 5 chars
        return "", fi.parseErrorf( "End of PDF file without content\n" )
    }
    if fi.buffer[ i ] != '%' || fi.buffer[ i+1 ] < 0x80 || fi.buffer[ i+2 ] < 0x80 ||
       fi.buffer[ i+3 ] < 0x80 || fi.buffer[ i+4 ] < 0x80 {
        if fi.verbose {
            fmt.Fprintf( os.Stderr, "Warning: recommended binary comment is missing\n" )
        }
    } else {
        fi.offset = skipToEOL( fi.buffer, i + 5 )
    }
    return string(fi.buffer[:8]), nil
}

func (fi *fileInput) parse( ) ( *PdfFile, error ) {

    pf := new( PdfFile )
    if err := pf.parseHeader( fi ); err != nil {
        return nil, fmt.Errorf( "PDF Parser: incorrect file header: %v", err )
    }
/* file structure: 
   header body XREF trailer [ update-body update-XREF update-trailer ]*

   The header is processed first and it does not have the correct signature
   parsing stops.

   The remaining of the file is processed as blocks:
   1 block is [ XREF + trailer + next body ]
   except for the last block which is not followed by a body:

   body XREF trailer [ update-body update-XREF update-trailer ]*
        ^                           ^
        |<----- regular block ----->|<------last block ------>|
    xrefstart                   blockend = previous xrefstart

   The initial body is processed separately. Objects in updated bodies
   have a higher generation number and replace the previous definitions
   that have a lower generation number (all initial objects start at
   generation 0). Since parsing starts always from the latest update, when
   adding objects, if an object exists already in the accumulated objects,
   it is simply ignored.

   A trailer has a dictionary and is followed by a xrefstart pointing at
   the XREF associated with the trailer and an EOF comment. If the trailer
   is not the initial one, it has also a value in the directory giving
   the offset of the previous xrefstart (prev).

   The last updated xrefstart is searched directly from the end of the
   file (huntForXref), and blocks are then processed until the initial
   one is reached.
*/
    mainObjStart := int64(fi.offset)
//    fmt.Printf( "First object offset : %d\n", mainObjStart )

    xrefStart, err := huntForXref( fi ) // latest updated XREF pointer
    if err != nil {
        return nil, fmt.Errorf( "PDF Parser: cannot find XREF: %v", err )
    }
//    fmt.Printf( "XREF starts @ 0x%x\n", xrefStart )

    pf.ObjById = make( map[int64]*PdfObject )

    blockEnd := fi.size                 // latest updated block end
    var objEnd int64
    for i := 0; ; i++ {                 // from last block (0) to first
        objEnd = xrefStart              // xref follows an object body
        if err = pf.parseXrefTable( fi, xrefStart, blockEnd ); err != nil {
            return nil, fmt.Errorf( "PDF Parser: invalid XREF table: %v", err )
        }
        blockEnd = xrefStart            // ready for previous update  block
        xrefStart, err = pf.parseTrailer( fi )
        if err != nil {
            return nil, fmt.Errorf( "PDF Parser: invalid trailer: %v", err )
        }
        if xrefStart == 0 { break }     // no more updates, process main body

        if i > 0 {                      // not the last, process following body
            err = pf.parseObjects( fi, 0, 0 ) // keep moving in the same buffer
            if err != nil {
                return nil, fmt.Errorf( "PDF Parser: invalid body: %v", err )
            }
        }
    }
//    fmt.Printf( "End object offset : 0x%x\n", objEnd )
    err = pf.parseObjects( fi, mainObjStart, objEnd )
    if err != nil {
        return nil, fmt.Errorf( "PDF Parser: invalid body: %v", err )
    }
    return pf, nil
}

func newFileInput( path string, verbose, fix bool ) ( *fileInput, error ) {
    fd, err := os.Open( path )
    if err != nil {
		return nil, fmt.Errorf( "Unable to open file %s: %v", path, err )
	}

    var fi fileInput
    fi.fd = fd

    fs, _ := fd.Stat()                  // Get total file size
    fi.size = fs.Size()

    fi.buffer = make( []byte, 512, INPUT_BUFFER_SIZE )
    fi.verbose = verbose
    fi.fix = fix
    return &fi, nil
}

type ParseArgs struct {
    Path    string      // original file to process (must be given)
    Verbose bool        // verbose parsing (false by default)
    Fix     bool        // fix during parsing (stop with error by default)
}

func Parse( args *ParseArgs ) ( *PdfFile, error ) {

    fi, err := newFileInput( args.Path, args.Verbose, args.Fix )
    if err != nil { return nil, err }
    defer fi.fd.Close()

    pdf, err := fi.parse( )
    if err != nil { return nil, err }

    return pdf, nil
}
