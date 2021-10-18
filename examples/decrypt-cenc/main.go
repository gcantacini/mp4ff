package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/edgeware/mp4ff/aac"
	"github.com/edgeware/mp4ff/mp4"
)

func main() {

	inFilePath := flag.String("i", "", "Required: Path to input file")
	outFilePath := flag.String("o", "", "Required: Output file")
	hexKey := flag.String("k", "", "Required: key (hex)")

	err := start(*inFilePath, *outFilePath, *hexKey)
	if err != nil {
		log.Fatalln(err)
	}

}

func start(inPath, outPath, hexKey string) error {

	ifh, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer ifh.Close()

	if len(hexKey) != 32 {
		return fmt.Errorf("Hex key must have length 32 chars")
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return err
	}

	err = decryptCenc(ifh, key, outPath)
	if err != nil {
		return err
	}
	return nil
}

func decryptCenc(r io.Reader, key []byte, outPath string) error {
	inMp4, err := mp4.DecodeFile(r)
	if err != nil {
		return err
	}
	if !inMp4.IsFragmented() {
		return fmt.Errorf("file not fragmented")
	}

	outPathVideo := outPath + ".m4v"
	outPathAudio := outPath + ".m4a"

	for _, trak := range inMp4.Init.Moov.Traks {
		trackID := trak.Tkhd.TrackID
		stsd := trak.Mdia.Minf.Stbl.Stsd
		var encv *mp4.VisualSampleEntryBox
		var enca *mp4.AudioSampleEntryBox
		var ofh *os.File
		for _, child := range stsd.Children {
			switch child.Type() {
			case "encv":
				encv = child.(*mp4.VisualSampleEntryBox)
				ofh, err = os.Create(outPathVideo)
				if err != nil {
					return err
				}
			case "enca":
				enca = child.(*mp4.AudioSampleEntryBox)
				ofh, err = os.Create(outPathAudio)
				if err != nil {
					return err
				}
			default:
				continue
			}
		}
		var trex *mp4.TrexBox
		for _, trex = range inMp4.Init.Moov.Mvex.Trexs {
			if trex.TrackID == trackID {
				break
			}
		}
		if encv != nil {
			err = createVideoInit(encv, trak, ofh)
			if err != nil {
				return err
			}
		}
		if enca != nil {
			err = createAudioInit(enca, trak, ofh)
			if err != nil {
				return err
			}
		}
		err = decodeSegments(inMp4, trak, trex, key, ofh)
		if err != nil {
			return err
		}
		ofh.Close()
	}

	return nil
}

func createVideoInit(encv *mp4.VisualSampleEntryBox, trak *mp4.TrakBox, w io.Writer) error {
	sinf := encv.Sinf
	if sinf.Frma.DataFormat != "avc1" && sinf.Frma.DataFormat != "avc3" {
		return fmt.Errorf("frma %s not supported", sinf.Frma.DataFormat)
	}
	if sinf.Schm.SchemeType != "cenc" {
		return fmt.Errorf("scheme type %s not supported", sinf.Schm.SchemeType)
	}
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(trak.Mdia.Mdhd.Timescale, "video", "und")
	err := init.Moov.Trak.SetAVCDescriptor("avc1", encv.AvcC.SPSnalus, encv.AvcC.PPSnalus)
	if err != nil {
		return err
	}
	err = init.Encode(w)
	if err != nil {
		return err
	}
	return nil
}

func createAudioInit(enca *mp4.AudioSampleEntryBox, trak *mp4.TrakBox, w io.Writer) error {
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(trak.Mdia.Mdhd.Timescale, "audio", trak.Mdia.Mdhd.GetLanguage())
	samplingFrequency := enca.SampleRate
	err := init.Moov.Trak.SetAACDescriptor(aac.AAClc, int(samplingFrequency))
	if err != nil {
		return err
	}
	err = init.Encode(w)
	if err != nil {
		return err
	}
	return nil
}

func decodeSegments(f *mp4.File, trak *mp4.TrakBox, trex *mp4.TrexBox, key []byte, w io.Writer) error {
	outNr := uint32(1)
	for i, inSeg := range f.Segments {
		for j, inFrag := range inSeg.Fragments {
			fmt.Printf("Segment %d, fragment %d\n", i+1, j+1)
			outSeg := mp4.NewMediaSegment()
			frag, err := mp4.CreateFragment(outNr, mp4.DefaultTrakID)
			if err != nil {
				return err
			}
			outSeg.AddFragment(frag)
			outSamples, err := decodeFragment(inFrag, trak, trex, key)
			if err != nil {
				return err
			}
			for i := range outSamples {
				frag.AddFullSample(outSamples[i])
			}
			err = outSeg.Encode(w)
			if err != nil {
				return err
			}
			outNr++
		}
	}

	return nil
}

func decodeFragment(frag *mp4.Fragment, trak *mp4.TrakBox, trex *mp4.TrexBox, key []byte) ([]mp4.FullSample, error) {
	moof := frag.Moof
	traf := findTraf(moof, trak.Tkhd.TrackID)
	//defaultSampleInfoSize := traf.Saiz.DefaultSampleInfoSize
	//saizSampleCount := traf.Saiz.SampleCount
	//saioOffset := traf.Saio.Offset
	senc := traf.Senc
	samples, err := frag.GetFullSamples(trex)
	if err != nil {
		return nil, err
	}
	outSamples := make([]mp4.FullSample, 0, len(samples))

	// TODO. Interpret saio and saiz to get to the right place
	// Saio tells where the IV starts relative to moof start
	// It typically ends up inside saiz (16 bytes after start)
	for i := range samples {
		decSample, err := decryptSample(uint32(i), samples, key, senc)
		if err != nil {
			return nil, err
		}
		outSamples = append(outSamples, decSample)
	}
	return outSamples, nil
}

func findTraf(moof *mp4.MoofBox, trackID uint32) *mp4.TrafBox {
	for _, traf := range moof.Trafs {
		if traf.Tfhd.TrackID == trackID {
			return traf
		}
	}
	panic("no matching traf found")
}

func decryptSample(i uint32, samples []mp4.FullSample, key []byte, senc *mp4.SencBox) (mp4.FullSample, error) {
	data := samples[i].Data
	var iv []byte
	if len(senc.IVs[i]) == 8 {
		iv = make([]byte, 0, 16)
		iv = append(iv, senc.IVs[i]...)
		iv = append(iv, []byte{0, 0, 0, 0, 0, 0, 0, 0}...)
	} else {
		iv = senc.IVs[i]
	}

	outData := make([]byte, 0, len(data))
	if len(senc.SubSamples) != 0 {
		ss := senc.SubSamples[i]
		var pos uint32 = 0
		for j := 0; j < len(ss); j++ {
			nrClear := uint32(ss[j].BytesOfClearData)
			nrEnc := ss[j].BytesOfProtectedData
			outData = append(outData, data[pos:pos+nrClear]...)
			pos += nrClear
			cryptOut, err := mp4.DecryptSampleCTR(data[pos:pos+nrEnc], key, iv)
			if err != nil {
				return mp4.FullSample{}, err
			}
			outData = append(outData, cryptOut...)
			pos += nrEnc
		}
	} else {
		cryptOut, err := mp4.DecryptSampleCTR(data, key, iv)
		if err != nil {
			return mp4.FullSample{}, err
		}
		outData = append(outData, cryptOut...)
	}
	outFull := mp4.FullSample{
		Sample:     samples[i].Sample,
		DecodeTime: samples[i].DecodeTime,
		Data:       outData,
	}
	return outFull, nil
}
