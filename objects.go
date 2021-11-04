
package pdf

import (
    "fmt"

)

func (pf *PdfFile) PrintFileIds( ) {
    if pf.Id == nil {
        fmt.Printf( "no file IDs\n" )
    } else {
        fmt.Printf( "PDF file IDs: 0x%x, 0x%x\n", pf.Id.data[0].(pdfHexString), pf.Id.data[1].(pdfHexString) )
    }
}

func (pf *PdfFile) PrintFileTrailer( ) {
    fmt.Printf( "Trailer: " )
    printValue( pf.Trailer, "  " )
}

func (pf *PdfFile) PrintEncryption( ) {
    pf.printReference( &(pf.Encrypt), "File Encryption" )
}

func (pf *PdfFile) PrintInfo( ) {
    pf.printReference( &(pf.Info), "File Info" )
}

func (pf *PdfFile) PrintCatalog( ) {
    pf.printReference( &(pf.Catalog), "Root catalog" )
/*
    if pf.Catalog.id == 0 {
        fmt.Printf("No root catalog defined\n")
        return
    }
    catalog, ok := pf.ObjById[ pf.Catalog.id ]
    if ! ok {
        fmt.Printf( "Root catalog does not refer to any existing object\n" )
        return
    }
    if catalog.gen != pf.Catalog.gen {
        fmt.Printf( "Root catalog generation number (%d) does not match existing object (%d)\n",
                    pf.Catalog.gen, catalog.gen )
        return
    }
    fmt.Printf( "Root catalog " )
    catalog.print()
*/
}

/*
func (pf *pdf.PdfFile)printEncryptionData(  ) {
    if ed := pf.Encrypt; ed != nil {
        fmt.Printf( "Encrypt object ID %d gen %d\n", ed.id, ed.generation )
        dic := ed.value.(pdf.pdfDictionary)
        for k, v := range dic.val {
            if k == "O" || k == "U" {
                fmt.Printf( "%s=%x\n", k, v )
            } else {
                fmt.Printf( "%s=%s\n", k, v )
            }
        }
    } else {
        fmt.Println( "Document is not encrypted" )
    }
}
*/

func (pf *PdfFile) printReference( ref *pdfReference, objName string ) {
    if ref.id == 0 {
        fmt.Printf( "No %s defined\n", objName )
        return
    }
    obj, ok := pf.ObjById[ ref.id ]
    if ! ok {
        fmt.Printf( "%s does not refer to any existing object\n", objName )
        return
    }
    if obj.gen != ref.gen {
        fmt.Printf( "%s generation number (%d) does not match existing object (%d)\n",
                    objName, ref.gen, obj.gen )
        return
    }
    fmt.Printf( "%s ", objName )
    obj.print()
}

func (obj *PdfObject) print( ) {
    fmt.Printf( "Id: %d gen: %d", obj.id, obj.gen )
    printValue( obj.value, "  " )
}

func printMapValue( val map[string]interface{}, header, indent string ) {
    fmt.Printf( header )
    for k, v := range( val ) {
        fmt.Printf( "%s /%s: ", indent, k )
        printValue( v, indent + "  " )
    }
}

func printValue( val interface{}, indent string ) {
    switch val := val.(type) {
    case pdfBool:
        fmt.Printf( " %t\n", val )
    case pdfNumber:
        fmt.Printf( " %g\n", val )
    case pdfString:
        fmt.Printf( " %s\n", val )
    case pdfHexString:
        fmt.Printf( " 0x%x\n", val )
    case pdfName:
        fmt.Printf( " /%s\n", val )
    case pdfDictionary:
//        fmt.Printf( " Dictionary:\n" )
        printMapValue( val.data, " Dictionary:\n", indent )
    case pdfStream:
        fmt.Printf( " Stream extent:" )
        printValue( val.extent, indent + "  " )
        fmt.Printf( "%s Stream length: %d\n", indent, len( val.data ) )
    case PdfArray:
        fmt.Printf(" Array:\n" )
        for i, v := range( val.data ) {
            fmt.Printf("%s   %d: ", indent, i )
            printValue( v, indent + "  " )
        }
    case pdfNull:
        fmt.Printf( " Null" )
    case pdfReference:
        fmt.Printf( " Indirect object reference: id %d generation %d\n",
                    val.id, val.gen )
    }
}

func (pf *PdfFile) PrintAllObjs( ) {
    for _, v := range pf.Objects {
        fmt.Printf( "Object: id %d generation %d:", v.id, v.gen )
        printValue( v.value, "  " )
    }
}

func (pf *PdfFile) PrintObjects( ) {
}

func (pf *PdfFile) newIndirectObject( id, gen int64, content interface{} ) *PdfObject {
    nio := new(PdfObject)
    nio.id = id
    nio.gen = gen
    nio.value = content

    pf.Objects = append(pf.Objects, nio )
    pf.ObjById[id] = nio

    return nio
}

