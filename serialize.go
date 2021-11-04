
package pdf

import (
    "fmt"
    "os"
    "io"
    "time"
    "strings"
    "strconv"
    "crypto/md5"
)

// Improved speed by using fmt.Fprintf only when necessary,
// combining several calls into a single one and by avoiding
// too many nested calls to the detriment of modularity in
// some cases.

func reportSerializeErrorf( format string, a ...interface{} ) {
    fmt.Fprintf( os.Stderr, "Error serializing pdf file: " )
    fmt.Fprintf( os.Stderr, format, a... ) 
    os.Exit(1)
}

func reportSerializeError( err error ) {
    fmt.Fprintf( os.Stderr, "Error serializing pdf file: %v", err )
    os.Exit(1)
}

// TODO: complete the function - currently not used
func (pdf *PdfFile) makeFileID( f *os.File ) string {
    h := md5.New()
    io.WriteString( h, f.Name() )
    io.WriteString( h, fmt.Sprintf( "%v", time.Now() ) )

    // not the complete file but the difference is a small constant
    pos, err := f.Seek( 0, os.SEEK_CUR )
    if err != nil {
        reportSerializeError( err )
    }
    io.WriteString( h, fmt.Sprintf( "%d", pos ) )
//    if pdf.Info != nil {
//        for k, v := range pdf.Info {
//            io.WriteString( h, 
//        }
//    }
    res := fmt.Sprintf( "<%x>", h.Sum(nil) )
    return res
}

func (pdf *PdfFile) serializeTrailer( f *os.File, lastId, xrefPos int64 ) {
    f.WriteString( "trailer\n<<\n" )
    for _, k := range pdf.Trailer.keys {
        switch k {
        case "Size":
            fmt.Fprintf( f, "/Size %d\n", lastId )
        case "Root":
            fmt.Fprintf( f, "/Root %d %d R\n", pdf.Catalog.id, pdf.Catalog.gen )
        case "Info":
            fmt.Fprintf( f, "/Info %d %d R\n", pdf.Info.id, pdf.Info.gen )
        case "Encrypt":
            fmt.Fprintf( f, "/Encrypt %d %d R\n", pdf.Encrypt.id, pdf.Encrypt.gen )
        case "Id":
            fmt.Fprintf( f, "/ID [<%X> <%X>]\n",
                     pdf.Id.data[0].(pdfHexString), pdf.Id.data[1].(pdfHexString) )
        default:
            fmt.Fprintf( f, "/%s ", k )
            serializeValue( f, pdf.Trailer.data[k] )
            f.WriteString( "\n" )
        }
    }
    fmt.Fprintf( f, ">>\nstartxref\n%d\n%%%%EOF\n", xrefPos )
}

func (pdf *PdfFile) serializeXREF( f *os.File ) (last int64, pos int64) {
    pos, err := f.Seek( 0, os.SEEK_CUR )
    if err != nil {
        reportSerializeError( err )
    }
    f.WriteString( "xref\n" )

    n := len(pdf.Objects)
    fmt.Fprintf( f, "%d %d\n", 0, n + 1 ) // including free object 0
    fmt.Fprintf( f, "%010d %05d f\r\n", 0, 65535 )
    for id := 1; id <= n; id++ {
        obj := pdf.ObjById[int64(id)]
        fmt.Fprintf( f, "%010d %05d n\r\n", obj.start, obj.gen )
    }
    return int64(n + 1), pos
}

func serializeNumber( f *os.File, n float64 ) {
    // PDF does not use exponents and does not want trailing 0s after the decimal
    // point either. One solution is to use %g and to de-normalize the exponent,
    // which allows to keep the maximum precision from the significand, a.k.a
    // mantissa, adding leading 0s as needed (negative exponents) while making
    // sure there is no non-significant trailing 0s after a decimal point.
    res := fmt.Sprintf( "%g", n )
    end := strings.IndexByte( res, 'e' )
    if end != -1 {
        // e.g.  1.234567e-03 =>  0.001234567 
        // or   -1.234567e-05 => -0.00001234567
        // or    1.234567e+02 =>  123.4567
//        fmt.Printf( "%f or %s\n", n, res )
        start := 0
        if res[0] == '-' { start = 1 }
        exp, _ := strconv.Atoi( res[end+1:] )
        ml := end - start -1 // excluding '.'
        pt := 1              // in res only, most cases
        if res[ start+1 ] != '.' {
            pt = 0
            ml ++  // fix mantissa length
        }
        nZ := - exp
        dp := 1     // assumes fRes decimal point in most cases
        if exp > 0 {
            nZ = exp - ml + 1
            if pt == 1 || exp >= ml - 1 {
                dp = 0  // suppress decimal point if not in source or no following digits
            }
        }
        fRes := make( []byte, start + dp + ml + nZ ) // 1 for '.' in fRes even if pt is 0 in res
//        fmt.Printf( "Exp: %d, start: %d, end: %d, ml: %d, pt: %d, dp: %d nZ: %d\n", exp, start, end, ml, pt, dp, nZ )
        if start > 0 { fRes[0] = '-' }
        if exp < 0 {
            fRes[start] = '0'
            fRes[start+1] = '.'
            next := start + 2
            for i := -1; i > exp; i-- {
                fRes[next] = '0'
                next ++
            }
            fRes[next] = res[start]
            next++
            start++
            for i := 1; i < ml; i++ {
                fRes[next] = res[start+i]
                next ++
            }
//            fmt.Printf("Fixed res: %s\n", fRes )
            fmt.Fprintf( f, "%s", fRes )
//            panic("stop\n")
        } else {
            fRes[start] = res[start]  // first digit
            next := start + 1
            if pt == 1 { next ++ }    // skip following '.' in res

            if dp == 0 { // no decimal point in final result: exp >= ml
                for i:= 1; i < ml; i++ {  // copy first ml digits
                    fRes[start+i] = res[next]
                    next ++
                }
                start += ml
                for i := 0; i < nZ; i++ { // then remaining 0s
                    fRes[start+i] = '0'
                    next ++
                }
            } else { // decimal point in final result: exp < ml
                panic( "Unexpected %g case\n" )
/*
                for i:= 1; i < exp; i++ { // copy first exp digits of ml
                    fRes[start+i] = res[next]
                    next ++
                }
                start += exp
                fRes[start] = '.'         // then decimal point
                start ++
                for i := 0; i < ml - exp; i++ { // then remaining ml
                    fRes[start+i] = res[next]
                    next ++
                }
*/
            }
            f.Write( fRes )
        }
    } else {
        f.WriteString( res )
    }

}

func serializeDictionary( f *os.File, keys []string, d map[string]interface{} ) {
    f.WriteString( "<<\n" )
// keeping the same order as in the parsed file
    for _, k := range keys {
        v, ok := d[k]
        if ! ok {
            panic( fmt.Sprintf( "Dictionary with no value for key %s\n", k ) )
        }
        fmt.Fprintf( f, "/%s ", k )
        serializeValue( f, v )
        f.WriteString( "\n" )
    }
    f.WriteString( ">>" )
}

// trying to mimic adobe behavior:
// items are separated by 1 space (" ")
// closing an array within an array is followed by \n (i.e. ]\n)
// all arrays are limited to 10 items on the same line,
// if the previous item was an array, skip the possibly following
// \n separator (if it was the 10th item on the line)
var arrayLevel int

func serializeArray( f *os.File, data []interface{} ) {
    f.WriteString( "[" )
    arrayLevel ++
    previousValueIsArray := false
    for i, d := range data {
        if i > 0 {
            sep := " "
            if i % 10 == 0 {
                if ! previousValueIsArray {
                    sep = "\n"
                } else {
                    sep = ""
                }
            }
            f.WriteString( sep )
        }
        serializeValue( f, d )
        if _, ok := d.(PdfArray); ok {
            previousValueIsArray = true
        } else {
            previousValueIsArray = false
        }
    }
    arrayLevel --

    if arrayLevel == 0 {
        f.WriteString( "]" )
    } else {
        f.WriteString( "]\n" ) // to check
    }
}

func serializeValue( f *os.File, v interface{} ) {
    switch v := v.(type) {
    case pdfBool:
        fmt.Fprintf( f, "%t", bool(v) )
    case pdfNumber:
        serializeNumber( f, float64(v) )
    case pdfString:
        f.WriteString( "(" )
        f.WriteString( string(v) )
        f.WriteString( ")" )
    case pdfHexString:
        fmt.Fprintf( f, "<%X>", string(v) )
    case pdfName:
        f.WriteString( "/" )
        f.WriteString( string(v) )
    case pdfDictionary:
        serializeDictionary( f, v.keys, v.data )
    case pdfStream:
        serializeDictionary( f, v.extent.keys, v.extent.data )
        f.WriteString( "\nstream\r\n" )
        f.Write( v.data )
        f.WriteString( "\r\nendstream" )
    case PdfArray:
        serializeArray( f, v.data )
    case pdfNull:
        f.WriteString( "null" )
    case pdfReference:
        fmt.Fprintf( f, "%d %d R", v.id, v.gen )
    }
}

func (pdf *PdfFile) serializeObjects( f *os.File ) {
    for _, obj := range pdf.Objects {
//        var err error
        obj.start, _ = f.Seek( 0, os.SEEK_CUR )
        fmt.Fprintf( f, "%d %d obj\n", obj.id, obj.gen )
        serializeValue( f, obj.value )
//        _, err = f.WriteString( "\nendobj\n" )
//        if err != nil {
//            reportSerializeError( err )
//        }
        f.WriteString( "\nendobj\n" )
    }
}

func (pdf *PdfFile) serializeFirstLine( f *os.File ) {
    _, err := f.WriteString( pdf.Header )
    if err != nil {
        reportSerializeError( err )
    }
    _, err = f.Write( []byte{ 0x0a, 0x25, 0xf6, 0xe4, 0xfc, 0xdf, 0x0a } )
    if err != nil {
        reportSerializeError( err )
    }
}

func (pdf *PdfFile)Serialize( name string ) {
    f, err := os.Create( name )
    if err != nil {
        reportSerializeError( err )
    }
    pdf.serializeFirstLine( f )
    pdf.serializeObjects( f )
    last, pos := pdf.serializeXREF( f )
    pdf.serializeTrailer( f, last, pos )
    f.Close( )
}

