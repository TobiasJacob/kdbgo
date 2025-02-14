package kdb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"reflect"
	"time"
	"unsafe"

	uuid "github.com/nu7hatch/gouuid"
)

var typeSize = []int{
	1: 1, 4: 1, 10: 1,
	2: 16,
	5: 2,
	6: 4, 8: 4, 13: 4, 14: 4, 17: 4, 18: 4, 19: 4,
	7: 8, 9: 8, 12: 8, 15: 8, 16: 8}

var typeReflect = []reflect.Type{
	1:  reflect.TypeOf([]bool{}),
	2:  reflect.TypeOf([]uuid.UUID{}),
	4:  reflect.TypeOf([]byte{}),
	5:  reflect.TypeOf([]int16{}),
	6:  reflect.TypeOf([]int32{}),
	7:  reflect.TypeOf([]int64{}),
	8:  reflect.TypeOf([]float32{}),
	9:  reflect.TypeOf([]float64{}),
	10: reflect.TypeOf([]byte{}),
	12: reflect.TypeOf([]time.Duration{}),
	13: reflect.TypeOf([]Month{}),
	14: reflect.TypeOf([]int32{}),
	15: reflect.TypeOf([]float64{}),
	16: reflect.TypeOf([]time.Duration{}),
	17: reflect.TypeOf([]int32{}),
	18: reflect.TypeOf([]int32{}),
	19: reflect.TypeOf([]int32{})}

func makeArray(vectype int8, veclen int) interface{} {
	switch vectype {
	case 1:
		return make([]bool, veclen)
	case 4, 10:
		return make([]byte, veclen)
	case 2:
		return make([]uuid.UUID, veclen)
	case 5:
		return make([]int16, veclen)
	case 6, 14, 17, 18, 19:
		return make([]int32, veclen)
	case 13:
		return make([]Month, veclen)
	case 16, 12:
		return make([]time.Duration, veclen)
	case 7:
		return make([]int64, veclen)
	case 8:
		return make([]float32, veclen)
	case 9, 15:
		return make([]float64, veclen)
	case 11:
		return make([]string, veclen)
	}

	return nil
}

func (h *ipcHeader) getByteOrder() binary.ByteOrder {
	var order binary.ByteOrder
	order = binary.LittleEndian
	if h.ByteOrder == 0x00 {
		order = binary.BigEndian
	}
	return order
}

func (h *ipcHeader) ok() bool {
	return h.ByteOrder == 0x01 && h.RequestType < 3 && h.Compressed < 0x02 && h.MsgSize > 9
}

// Decode deserialises data from src in q ipc format.
func Decode(src *bufio.Reader) (data *K, msgtype ReqType, e error) {
	var header ipcHeader
	e = binary.Read(src, binary.LittleEndian, &header)
	if e != nil {
		return nil, -1, errors.New("Failed to read message header:" + e.Error())
	}
	if !header.ok() {
		return nil, -1, errors.New("header is invalid")
	}
	// try to buffer entire message in one go
	src.Peek(int(header.MsgSize - 8))

	var order = header.getByteOrder()
	if header.Compressed == 0x01 {
		compressed := make([]byte, header.MsgSize-8)
		_, e = io.ReadFull(src, compressed)
		if e != nil {
			return nil, header.RequestType, errors.New("Decode:readcompressed error - " + e.Error())
		}
		var uncompressed = Uncompress(compressed)
		var buf = bufio.NewReader(bytes.NewReader(uncompressed[8:]))
		data, e = readData(buf, order)
		return data, header.RequestType, e
	}
	data, e = readData(src, order)
	return data, header.RequestType, e
}

func readData(r *bufio.Reader, order binary.ByteOrder) (kobj *K, err error) {
	var msgtype int8
	err = binary.Read(r, order, &msgtype)
	if err != nil {
		return nil, err
	}
	switch msgtype {
	case -KB:
		var b byte
		binary.Read(r, order, &b)
		return &K{msgtype, NONE, b != 0x0}, nil

	case -UU:
		var u uuid.UUID
		binary.Read(r, order, &u)
		return &K{msgtype, NONE, u}, nil

	case -KG, -KC:
		var b byte
		binary.Read(r, order, &b)
		return &K{msgtype, NONE, b}, nil
	case -KH:
		var sh int16
		binary.Read(r, order, &sh)
		return &K{msgtype, NONE, sh}, nil

	case -KI, -KD, -KU, -KV:
		var i int32
		binary.Read(r, order, &i)
		return &K{msgtype, NONE, i}, nil
	case -KJ:
		var j int64
		binary.Read(r, order, &j)
		return &K{msgtype, NONE, j}, nil
	case -KE:
		var e float32
		binary.Read(r, order, &e)
		return &K{msgtype, NONE, e}, nil
	case -KF, -KZ:
		var f float64
		binary.Read(r, order, &f)
		return &K{msgtype, NONE, f}, nil
	case -KS:
		line, err := r.ReadBytes(0)
		if err != nil {
			return nil, err
		}
		str := string(line[:len(line)-1])

		return &K{msgtype, NONE, str}, nil
	case -KP:
		var ts time.Duration
		binary.Read(r, order, &ts)
		return &K{msgtype, NONE, qEpoch.Add(ts)}, nil
	case -KM:
		var m Month
		binary.Read(r, order, &m)
		return &K{msgtype, NONE, m}, nil
	case -KN:
		var span time.Duration
		binary.Read(r, order, &span)
		return &K{msgtype, NONE, span}, nil
	case KB, UU, KG, KH, KI, KJ, KE, KF, KC, KP, KM, KD, KN, KU, KV, KT, KZ:
		var vecattr Attr
		err = binary.Read(r, order, &vecattr)
		if err != nil {
			return nil, errors.New("readData: Failed to read vecattr:" + err.Error())
		}
		var veclen uint32
		err = binary.Read(r, order, &veclen)
		if err != nil {
			return nil, errors.New("Reading vector length failed -> " + err.Error())
		}
		var arr interface{}
		if msgtype >= KB && msgtype <= KT {
			bytedata := make([]byte, int(veclen)*typeSize[msgtype])
			_, err = io.ReadFull(r, bytedata)
			if err != nil {
				return nil, errors.New("Not enough data - " + err.Error())
			}
			head := (*reflect.SliceHeader)(unsafe.Pointer(&bytedata))
			head.Len = int(veclen)
			head.Cap = int(veclen)
			arr = reflect.Indirect(reflect.NewAt(typeReflect[msgtype], unsafe.Pointer(&bytedata))).Interface()
		} else {
			arr = makeArray(msgtype, int(veclen))
			err = binary.Read(r, order, arr)
		}
		if err != nil {
			return nil, errors.New("Error during conversion -> " + err.Error())
		}
		if msgtype == KC {
			return &K{msgtype, vecattr, string(arr.([]byte))}, nil
		}
		if msgtype == KP {
			arr := arr.([]time.Duration)
			var timearr = make([]time.Time, veclen)
			for i := 0; i < int(veclen); i++ {
				timearr[i] = qEpoch.Add(arr[i])
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		if msgtype == KD {
			arr := arr.([]int32)
			var timearr = make([]time.Time, veclen)
			for i := 0; i < int(veclen); i++ {
				d := time.Duration(arr[i]) * 24 * time.Hour
				timearr[i] = qEpoch.Add(d)
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		if msgtype == KZ {
			arr := arr.([]float64)
			var timearr = make([]time.Time, veclen)
			for i := 0; i < int(veclen); i++ {
				d := time.Duration(86400000*arr[i]) * time.Millisecond
				timearr[i] = qEpoch.Add(d)
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		if msgtype == KU {
			arr := arr.([]int32)
			var timearr = make([]Minute, veclen)
			for i := 0; i < int(veclen); i++ {
				d := time.Duration(arr[i]) * time.Minute
				timearr[i] = Minute(time.Time{}.Add(d))
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		if msgtype == KV {
			arr := arr.([]int32)
			var timearr = make([]Second, veclen)
			for i := 0; i < int(veclen); i++ {
				d := time.Duration(arr[i]) * time.Second
				timearr[i] = Second(time.Time{}.Add(d))
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		if msgtype == KT {
			arr := arr.([]int32)
			var timearr = make([]Time, veclen)
			for i := 0; i < int(veclen); i++ {
				timearr[i] = Time(qEpoch.Add(time.Duration(arr[i]) * time.Millisecond))
			}
			return &K{msgtype, vecattr, timearr}, nil
		}
		return &K{msgtype, vecattr, arr}, nil
	case K0:
		var vecattr Attr
		err = binary.Read(r, order, &vecattr)
		if err != nil {
			return nil, errors.New("readData: Failed to read vecattr ->" + err.Error())
		}
		var veclen uint32
		err = binary.Read(r, order, &veclen)
		if err != nil {
			return nil, errors.New("Reading vector length failed -> " + err.Error())
		}
		var arr = make([]*K, veclen)
		for i := 0; i < int(veclen); i++ {
			v, err := readData(r, order)
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return &K{msgtype, vecattr, arr}, nil
	case KS:
		var vecattr Attr
		err = binary.Read(r, order, &vecattr)
		if err != nil {
			return nil, errors.New("readData: Failed to read vecattr ->" + err.Error())
		}
		var veclen uint32
		err = binary.Read(r, order, &veclen)
		if err != nil {
			return nil, errors.New("Reading vector length failed -> " + err.Error())
		}
		var arr = makeArray(msgtype, int(veclen)).([]string)
		for i := 0; i < int(veclen); i++ {
			line, err := r.ReadSlice(0)
			if err != nil {
				return nil, err
			}
			arr[i] = string(line[:len(line)-1])
		}
		return &K{msgtype, vecattr, arr}, nil
	case XD, SD:
		dk, err := readData(r, order)
		if err != nil {
			return nil, err
		}
		dv, err := readData(r, order)
		if err != nil {
			return nil, err
		}
		res := NewDict(dk, dv)
		if msgtype == SD {
			res.Attr = SORTED
		}
		return res, nil
	case XT:
		var vecattr Attr
		err = binary.Read(r, order, &vecattr)
		if err != nil {
			return nil, errors.New("readData: Failed to read vecattr" + err.Error())
		}
		d, err := readData(r, order)
		if err != nil {
			return nil, err
		}
		if d.Type != XD {
			return nil, errors.New("expected dict")
		}
		dict := d.Data.(Dict)
		colNames := dict.Key.Data.([]string)
		colValues := dict.Value.Data.([]*K)
		return &K{msgtype, vecattr, Table{colNames, colValues}}, nil

	case KFUNC:
		var f Function
		line, err := r.ReadSlice(0)
		if err != nil {
			return nil, err
		}
		f.Namespace = string(line[:len(line)-1])
		b, err := readData(r, order)
		if err != nil {
			return nil, err
		}
		if b.Type != KC {
			return nil, errors.New("expected string")
		}
		f.Body = b.Data.(string)
		return &K{msgtype, NONE, f}, nil
	case KFUNCUP, KFUNCBP, KFUNCTR:
		var primitiveidx byte
		err = binary.Read(r, order, &primitiveidx)
		if err != nil {
			return nil, err
		}
		return &K{msgtype, NONE, primitiveidx}, nil
	case KPROJ, KCOMP:
		var n uint32
		err = binary.Read(r, order, &n)
		if err != nil {
			return nil, err
		}
		var res = make([]*K, n)
		for i := 0; i < len(res); i++ {
			res[i], err = readData(r, order)
			if err != nil {
				return nil, err
			}
		}
		return &K{msgtype, NONE, res}, nil
	case KEACH, KOVER, KSCAN, KPRIOR, KEACHRIGHT, KEACHLEFT:
		res, err := readData(r, order)
		if err != nil {
			return nil, err
		}
		return &K{msgtype, NONE, res}, nil
	case KDYNLOAD:
		// 112 - dynamic load
		return nil, errors.New("type is unsupported")
	case KERR:
		line, err := r.ReadSlice(0)
		if err != nil {
			return nil, err
		}
		errmsg := string(line[:len(line)-1])
		return nil, errors.New(errmsg)
	}
	return nil, ErrBadMsg
}

func ReadFromBuffer(data *bytes.Buffer) (*K, error) {
	var order = binary.LittleEndian
	reader := bufio.NewReader(data)
	// Read 0xFF, 0x01 bytes magic number
	reader.ReadByte()
	reader.ReadByte()
	return readData(reader, order)
}

func ReadFromFile(filename string) (*K, error) {
	var order = binary.LittleEndian
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(f)
	// Read 0xFF, 0x01 bytes magic number
	reader.ReadByte()
	reader.ReadByte()
	return readData(reader, order)
}
