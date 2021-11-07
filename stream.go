
package pdf

import (
    "fmt"
    "github.com/jrm-1535/jpeg"
)

func checkASCIIHexDecode( data []byte, verbose, fix bool ) ([]byte, error) {
    dl := 0         // decoded length
    offset := 0     // encoded offset, used to find the nibble parity
    var val byte    // use to accumulate 2 decoded Hex chars as 1 byte
    output := make( []byte, 2 * len(data) ) // pre-allocated for an upper bound

decodeLoop:
    for i, v := range data {
        switch v {
        case '>':
            break decodeLoop   // EOD
        case 0, '\t', '\n', '\f', '\r', ' ':
            // just skip any 'white-space' character (do not update offset)
        default:
            nib := makeNibbleFromHexChar( v )
            if nib == 0xff {
                if verbose {
                    fmt.Printf( "Invalid hex character 0x%x at offset %d\n", v, i )
                }
                return output[0:dl], fmt.Errorf( "Invalid hex character 0x%x in stream\n", v )
            }
            if offset & 1 == 0 { // first nibble, temporarily stored in val
                val = nib
            } else {
                output[dl] = (val << 4) + nib
                dl ++
            }
            offset++

        }
    }
    if dl & 1 == 1 {
        if verbose {
            fmt.Printf( "Odd number of nibbles, last nibble assumed to be 0\n" )
        }
        output[dl] = val << 4
        dl ++
    }
    if verbose {
        fmt.Printf( "Decoded length: %d\n", dl )
    }
    return output[0:dl], nil
}

func checkASCII85Decode( data []byte, verbose, fix bool ) ([]byte, error) {

    dl := 0         // decoded length
    gi := 0         // index in a group 0f 5 ASCII-85 chars
    n64 := int64(0) // number resulting from 5 ASCII-85 char (may not fit in 32 bits)
    offset := 0     // last offset in encoded data when EOD is found
    // pre-allocate an output buffer for an upper bound
    // At worst it is 4 * (len(data)-2) because of the 'z' compression for 0x00000000
    // At best it is 4 * ((len(data)-2) / 5) - if no 'z' character is used
    output := make( []byte, 4 * (len(data) - 2 ) )

decodeLoop:
    for i, v := range data {
        switch v {
        case '~':
            offset = i
            break decodeLoop   // starts EOD
        case 0, '\t', '\n', '\f', '\r', ' ':
            // just skip any 'white-space' character
        case 'z':
            if gi != 0 {
                if verbose {
                    fmt.Printf( "ASCII 85 character 'z' in a middle of a group at offset %d\n", i )
                }
                return output[0:dl], fmt.Errorf( "ASCII 85 character 'z' in a middle of a group in stream\n" )
            }
            dl += 4 // special case for 0x00000000
        default:
            if v < '!' || v > 'u' {
                if verbose {
                    fmt.Printf( "Invalid ASCII 85 character 0x%x at offset %d\n", v, i )
                }
                return output[0:dl], fmt.Errorf( "Invalid ASCII 85 character 0x%x in stream\n", v )
            }
            n64 = n64 * 85 + int64( v - '!' )
            if gi == 4 {    // we are about to process the 5th ASCI 85 char
                if n64 > 4294967295 {
                    if verbose {
                        fmt.Printf( "Invalid ASCII 85 encoding (beyond 2^32 -1) at offset %d\n", i )
                    }
                    return output[0:dl], fmt.Errorf( "Invalid ASCII 85 encoding (beyond 2^32 -1) in stream\n" )
                }
                output[dl] = byte(  n64 >> 24 )
                output[dl+1] = byte( (n64 >> 16) & 0xff )
                output[dl+2] = byte( (n64 >> 8) & 0xff )
                output[dl+3] = byte( n64 & 0xff )
                dl += 4
                gi = 0
                n64 = 0
            } else {
                gi ++
            }
        }
    }
    if len(data) <= offset + 1 || data[offset+1] != '>' {
        if verbose {
            fmt.Printf( "Invalid ASCII 85 EOD sequence (~ followed by 0x%x) at offset %d\n",
                        data[offset+1], offset+1 )
        }
        return output[0:dl], fmt.Errorf( "Invalid ASCII 85 EOD sequence (~ followed by 0x%x) in stream\n",
                             data[offset+1] )
    }
    if verbose {
        if gi != 0 {
            if gi == 1 {
                if verbose {
                    fmt.Printf( "Invalid ASCII 85 last group at offset %d\n", offset-1 )
                }
                return output[0:dl], fmt.Errorf( "Invalid ASCII last group in stream\n" )
            }
            // the last goup should be padded with as many 'u' as needed to make 5 chars
            for i := gi; i < 5; i++ {
                n64 = n64 * 85 + 84
            }
            if n64 > 4294967295 {
                if verbose {
                    fmt.Printf( "Invalid ASCII 85 encoding (beyond 2^32 -1) in last group at offset %d\n",
                                offset-1 )
                }
                return output[0:dl], fmt.Errorf( "Invalid ASCII 85 encoding (beyond 2^32 -1) in last group\n" )
            }
            for i := 0; i < gi-1; i++ {
                output[dl] = byte( 0xff & ( n64 >> uint( 24 - i * 8 ) ) )
                dl++
            }
        }
        fmt.Printf( "Decoded ASCII 85 data length: %d\n", dl )
    }
    return output[0:dl], nil
}

/*
The encoded data is a sequence of runs, where each run consists of a length byte
followed by 1 to 128 bytes of data. If the length byte is in the range 0 to 127,
the following length + 1 (1 to 128) bytes are copied literally during decompression.
If length is in the range 129 to 255, the following single byte is to be copied
257 − length (2 to 128) times during decompression. A length value of 128 denotes EOD.
*/
func checkRunLengthDecode( data []byte, verbose, fix bool ) error {
    dl := 0
    offset := 0
    maxOffset := len(data) - 1
    for {
        if offset > maxOffset {
            if verbose {
                fmt.Printf( "Reached the end of stream without end of runlength at offset %d\n",
                            offset )
            }
            return fmt.Errorf( "Reached the end of stream without end of runlength\n" )
        }
        rl := int(data[offset])
        if rl < 128 {
            dl += rl + 1               // actual "decoded" data
            nOffset := offset + rl + 2 // rl offset + 1 to get to the first following byte + actual (rl + 1)
            if nOffset > len(data) { 
                if verbose {
                    fmt.Printf( "Invalid runlength encoding (beyond end of stream) at offset %d\n",
                                offset )
                }
                return fmt.Errorf( "Invalid runlength encoding (beyond end of stream)\n" )
            }
            offset = nOffset
        } else if rl == 128 {
            break
        } else {
            dl += 257 - rl
            offset += 2
        }
    }
    if verbose {
        fmt.Printf( "Decoded runlength data length: %d\n", dl )
    }
    return nil
}

// TODO: add CCITTFaxDecode and FlatDecode

func checkCCITTFaxDecode( data []byte, parameters map[string]interface{}, verbose, fix bool ) ([]byte, error) {
    return []byte{}, fmt.Errorf( "checking CCITTFaxDecode is not supported yet\n" )
}


func checkDCTDecode( data []byte, verbose, fix bool ) ([]byte, jpeg.Metadata, error) {

    // DCTDecode (JPEG) should be the last decoder in any sequence of decoders
    // since the decompressed data is an image to present.
    if verbose {
        fmt.Printf( "checkDCTDecode first 4 bytes 0x%x\n", string(data[0:4]) )
    }
    var control jpeg.Control = jpeg.Control{ Content:verbose, Fix:fix }
    jpg, err := jpeg.Analyze( data, &control )
    if err != nil {
        return []byte{}, jpeg.Metadata{}, err
    }

    if jpg == nil || ! jpg.IsComplete( ) {
        return []byte{}, jpeg.Metadata{}, fmt.Errorf( "JPEG data cannot be parsed\n" )
    }

    actualL, dataL := jpg.GetActualLengths()
    if verbose {
        fmt.Printf( "Actual JPEG length: %d (data length: %d)\n", actualL, dataL )
    }
    if fix {    // FIXME: does not work if other decoders are used before checkDCTDecode (unlikely)
        var fixed []byte
        fixed, err = jpg.Generate()
        if err == nil {
            data = fixed
            fmt.Printf( "Fixing jpeg stream (len=%d)\n", len(data) )
        }
    }
    return data, jpg.GetMetadata(), err
}

func makeOneElementArray( element interface{} ) *PdfArray {
    newArray := new( PdfArray )
    newArray.data = make( []interface{}, 1 )
    newArray.data[0] = element
    return newArray
}

func checkStream( stream *pdfStream, verbose, fix bool ) error {
    dic := stream.extent

    if filter, ok := dic.data["Filter"]; ok {
// filter may be a simple name or an array of names.
// to normalise the processing, an array is created for the single name case
// Similarly, if DecodeParms is defined, it is a simple pdfDictionary if
// filter is a simple name, or an array of pdfDictionaries otherwise. Again,
//  we normalize those 2 cases by creating an array with the single dictionary.
        var filters PdfArray
        var fParams PdfArray

        if f, ok := filter.(pdfName); ok {
            filters = *makeOneElementArray( f )
            if parms, ok := dic.data["DecodeParms"]; ok {
                fParams = *makeOneElementArray( parms )
            }
        } else {    // filter is an array, so is DecodeParms
//            filterArray := filter.(PdfArray)
            filters = filter.(PdfArray)
            if parms, ok := dic.data["DecodeParms"]; ok {
//                parmsArray := parms.(PdfArray)
                fParams = parms.(PdfArray)
            }
        }

        data := stream.data
        for i, v := range filters.data {
            if verbose {
                fmt.Printf( "Stream Filter: %v\n", v.(pdfName) )
                if fParams.data != nil {
                    if p, ok := fParams.data[i].(pdfDictionary); ok {
                        printMapValue( p.data, "    Parameters:\n", "    " )
//                        fmt.Printf( ">>> Parameters: %v\n", p.data )
                    } // else if pdfNull, ignore
                }
            }
            var err error
            switch v.(pdfName) {
            case "DCTDecode":
                var meta jpeg.Metadata
                data, meta, err = checkDCTDecode( data, verbose, fix )
                if err == nil && fix {  // update stream if jpeg could have fixed it
//                    fmt.Printf( "Meta bpc=%d, w=%d h=%d\n", meta.SampleSize, meta.Width, meta.Height )
                    stream.data = data
                    dic.data["BitsPerComponent"] = pdfNumber(meta.SampleSize)
                    dic.data["Width"] = pdfNumber(meta.Width)
                    dic.data["Height"] = pdfNumber(meta.Height)
                    dic.data["Length"] = pdfNumber(len(stream.data))
                }
            case "ASCIIHexDecode":
                data, err = checkASCIIHexDecode( data, verbose, fix )
            // TODO: no other case is currently supported
            }
            if err != nil {
                return err
            }
        }
    } else if verbose {
        fmt.Printf( "No filter specified\n" )
    }
    return nil
}

func (pf *PdfFile)CheckStreams( verbose, fix bool ) error {
    for oIndex, _ := range pf.Objects {
        objPtr := pf.Objects[oIndex]
        if stream, ok := objPtr.value.(pdfStream); ok {
            if verbose {
                fmt.Printf( "Checking pdfStream ID %d, gen %d\n",
                            pf.Objects[oIndex].id, pf.Objects[oIndex].gen )
            }
            err := checkStream( &stream, verbose, fix )
            if err != nil {
                if verbose {
                    fmt.Printf( ">>> Error: %v", err )
                }
                return err
            }
            objPtr.value = stream   // checkStream may have modified the stream
        }
    }
    return nil
}

type StreamArgs struct {
    Verbose     bool        // verbose checking the embedded streams
    Fix         bool        // fix stream contents after parsing
}

func (pf *PdfFile)Check( args *StreamArgs ) error {
    return pf.CheckStreams( args.Verbose, args.Fix )
}

