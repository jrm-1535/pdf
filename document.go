
package pdf

import (
    "fmt"

)

const (
    DEFAULT_PAGE_NUMBER = 2
)


func newDocument( version int ) *PdfFile {
    pf := new(PdfFile)

    pf.Version = fmt.Sprintf( "1.%d", version )
    pf.Header = fmt.Sprintf( "%%PDF-1.%d\n%%%s", version, string([]byte{0xf6, 0xe4, 0xfc, 0xdf}) )

    pf.Objects = make( []*PdfObject, 0, 16 )
    pf.ObjById = make( map[int64]*PdfObject, 16 )

    var pA PdfArray
    pA.data = make( []interface{}, 0, DEFAULT_PAGE_NUMBER )

    pDict := make( map[string]interface{}, DEFAULT_DICTIONARY_SIZE )
    pDict["Type"] = pdfName("/Pages")
    pDict["Count"] = pdfNumber(0)
    pDict["Kids"] = pA
    pf.newIndirectObject( 1, 0, pDict )

    rDict := make( map[string]interface{}, DEFAULT_DICTIONARY_SIZE )
    rDict["Type"] = pdfName("/Catalog")
    rDict["Pages"] = pdfReference{ id: 1, gen: 0 }
    pf.newIndirectObject( 2, 0, rDict )

    pf.Trailer.keys = make( []string, 0, DEFAULT_DICTIONARY_SIZE )
    pf.Trailer.keys = append( pf.Trailer.keys, "Pages", "Catalog" )

    pf.Trailer.data = make( map[string]interface{}, DEFAULT_DICTIONARY_SIZE )
    pf.Trailer.data["Root"] = pdfReference{ id: 2, gen: 0 }

    pf.Size = 3     // including the head of free object list
    pf.Trailer.data["Size"] = pf.Size

    return pf
}

func (pf *PdfFile)addPage( ) error {
    return nil
}

