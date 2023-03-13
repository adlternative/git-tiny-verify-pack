package pack

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"

	gitzlib "github.com/adlternative/git-zlib-cgo"
)

const headerSize = 12
const Signature = 0x5041434b
const GitSha1Rawsz = 20

type PackFile struct {
	file       *os.File
	version    uint32
	objectNums uint32
	curOffset  uint32
	objects    []*Object

	inputBuf *buffer
}

func (pf *PackFile) fill(min uint32) ([]byte, error) {
	return pf.inputBuf.Fill(min)
}

func (pf *PackFile) buffer() []byte {
	return pf.inputBuf.Buffer()
}

func (pf *PackFile) use(length uint32) {
	pf.inputBuf.Use(length)
	pf.curOffset += length
}

func NewPackFile(packPath string) (*PackFile, error) {
	file, err := os.Open(packPath)
	if err != nil {
		return nil, err
	}
	return &PackFile{
		file:     file,
		inputBuf: newBuffer(file),
	}, nil
}

func (pf *PackFile) ParseHeader() error {
	header, err := pf.fill(headerSize)
	if err != nil {
		return err
	}
	defer pf.use(headerSize)

	if binary.BigEndian.Uint32(header[0:4]) != Signature {
		return fmt.Errorf("bad signature %v", header[0:4])
	}

	version := binary.BigEndian.Uint32(header[4:8])
	if version != 2 && version != 3 {
		return fmt.Errorf("bad version %d", version)
	}
	pf.version = version
	log.Printf("version = %d\n", version)
	objectNums := binary.BigEndian.Uint32(header[8:12])

	pf.objectNums = objectNums
	log.Printf("object nums = %d\n", objectNums)

	return nil
}

func (pf *PackFile) ParseObjects() error {
	for i := uint32(0); i < pf.objectNums; i++ {
		curOffset := pf.curOffset

		b, err := pf.readByte()
		if err != nil {
			return err
		}

		_type := ObjectType((b >> 4) & 7)
		size := uint64(b & 15)
		shift := 4

		for b&0x80 != 0 {
			b, err = pf.readByte()
			if err != nil {
				return err
			}

			size += (uint64(b) & 0x7f) << shift
			shift += 7
		}

		switch _type {
		case ObjRefDelta:
			_, err = pf.fill(GitSha1Rawsz)
			if err != nil {
				return err
			}

			// handle ref delta

			pf.use(GitSha1Rawsz)
		case ObjOfsDelta:
			b, err = pf.readByte()
			if err != nil {
				return err
			}

			baseOffset := b & 127
			for b&128 != 0 {
				baseOffset++
				if baseOffset == 0 {
					return fmt.Errorf("bad delta base object offset value")
				}

				if b, err = pf.readByte(); err != nil {
					return err
				}

				baseOffset = (baseOffset << 7) + (b & 127)
			}
			ofsOffset := curOffset - uint32(baseOffset)
			if ofsOffset <= 0 || ofsOffset >= curOffset {
				return fmt.Errorf("delta base offset is out out of bound")
			}

		//	// 读取 baseoffset 用当前对象的 offset 去减可以得到 base 的 offset
		//	base_offset = c & 127;
		//	while (c & 128) {
		//	base_offset += 1;
		//	if (!base_offset || MSB(base_offset, 7))
		//		bad_object(obj->idx.offset, _("offset value overflow for delta base object"));
		//	p = fill(1);
		//	c = *p;
		//	use(1);
		//	base_offset = (base_offset << 7) + (c & 127);
		//}
		//	*ofs_offset = obj->idx.offset - base_offset;
		//	if (*ofs_offset <= 0 || *ofs_offset >= obj->idx.offset)
		//		bad_object(obj->idx.offset, _("delta base offset is out of bound"));
		//	break;

		case ObjCommit, ObjTree, ObjBlob, ObjTag:
		default:
			return fmt.Errorf("bad type %v", _type)
			/*
					case OBJ_REF_DELTA:
					// 读取 ref_oid
					oidread(ref_oid, fill(the_hash_algo->rawsz));
					use(the_hash_algo->rawsz);
					break;
				case OBJ_OFS_DELTA:
					p = fill(1);
					c = *p;
					use(1);
					// 读取 baseoffset 用当前对象的 offset 去减可以得到 base 的 offset
					base_offset = c & 127;
					while (c & 128) {
						base_offset += 1;
						if (!base_offset || MSB(base_offset, 7))
							bad_object(obj->idx.offset, _("offset value overflow for delta base object"));
						p = fill(1);
						c = *p;
						use(1);
						base_offset = (base_offset << 7) + (c & 127);
					}
					*ofs_offset = obj->idx.offset - base_offset;
					if (*ofs_offset <= 0 || *ofs_offset >= obj->idx.offset)
						bad_object(obj->idx.offset, _("delta base offset is out of bound"));
					break;
				case OBJ_COMMIT:
				case OBJ_TREE:
				case OBJ_BLOB:
				case OBJ_TAG:
					break;
				default:
					bad_object(obj->idx.offset, _("unknown object type %d"), obj->type);

			*/
		}

		obj := &Object{
			offset: curOffset,
			_type:  _type,
			size:   size,
		}
		pf.objects = append(pf.objects, obj)

		log.Printf("index=%d offset=%d, type=%s, size=%d\n", i, obj.offset, obj._type, obj.size)
		// 创建一个缓冲区来存储解压后的数据
		_, err = pf.unpackEntryData(int(obj.size), obj._type)
		if err != nil {
			return err
		}

		//log.Printf("data=%s\n len=%d\n", uncompressedData, len(uncompressedData))
	}

	return nil
}

func (pf *PackFile) readByte() (byte, error) {
	buf, err := pf.fill(1)
	if err != nil {
		return 0, err
	}
	c := buf[0]
	pf.use(1)
	return c, nil
}

func (pf *PackFile) Close() error {
	return pf.file.Close()
}

func (pf *PackFile) unpackEntryData(size int, _type ObjectType) ([]byte, error) {
	var err error
	outBuf := make([]byte, size)
	zstream := &gitzlib.GitZStream{}
	status := gitzlib.Z_OK

	err = zstream.InflateInit()
	if err != nil {
		return nil, err
	}
	zstream.SetOutBuf(outBuf, size)

	for status == gitzlib.Z_OK {
		_, err = pf.fill(1)
		if err != nil {
			return nil, err
		}

		allInputBuf := pf.buffer()
		inputLength := len(allInputBuf)
		//log.Printf("curoff=%d, inputlen=%d curdata=%d", pf.curOffset, inputLength, allInputBuf[0])
		zstream.SetInBuf(allInputBuf, inputLength)

		status, err = zstream.Inflate(0)
		if err != nil {
			return nil, err
		}

		pf.use(uint32(inputLength - zstream.AvailIn()))
	}
	if status != gitzlib.Z_STREAM_END || zstream.TotalOut() != size {
		return nil, fmt.Errorf("inflate returned %d", status)
	}

	err = zstream.InflateEnd()
	if err != nil {
		return nil, err
	}

	return outBuf, nil
}
