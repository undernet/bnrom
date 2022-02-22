package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"os"
	"runtime"

	"github.com/schollz/progressbar/v3"
	"github.com/yumland/bnrom/paletted"
	"github.com/yumland/bnrom/sprites"
	"github.com/yumland/gbarom"
	"github.com/yumland/pngchunks"
	"golang.org/x/sync/errgroup"
)

type FrameInfo struct {
	BBox   image.Rectangle
	Origin image.Point
	Delay  int
	Action sprites.FrameAction
}

func processOne(idx int, anims []sprites.Animation) error {
	left := 0
	top := 0

	var infos []FrameInfo
	var fullPalette color.Palette
	spriteImg := image.NewPaletted(image.Rect(0, 0, 2048, 2048), nil)

	for _, anim := range anims {
		for _, frame := range anim.Frames {
			fullPalette = frame.Palette

			var frameInfo FrameInfo
			frameInfo.Delay = int(frame.Delay)
			frameInfo.Action = frame.Action

			img := frame.MakeImage()
			spriteImg.Palette = img.Palette

			trimBbox := paletted.FindTrim(img)

			frameInfo.Origin.X = img.Rect.Dx()/2 - trimBbox.Min.X
			frameInfo.Origin.Y = img.Rect.Dy()/2 - trimBbox.Min.Y

			if left+trimBbox.Dx() > spriteImg.Rect.Dx() {
				left = 0
				top = paletted.FindTrim(spriteImg).Max.Y
				top++
			}

			frameInfo.BBox = image.Rectangle{image.Point{left, top}, image.Point{left + trimBbox.Dx(), top + trimBbox.Dy()}}

			draw.Draw(spriteImg, frameInfo.BBox, img, trimBbox.Min, draw.Over)
			infos = append(infos, frameInfo)

			left += trimBbox.Dx() + 1
		}
	}

	if spriteImg.Palette == nil {
		return nil
	}

	subimg := spriteImg.SubImage(paletted.FindTrim(spriteImg))
	if subimg.Bounds().Dx() == 0 || subimg.Bounds().Dy() == 0 {
		return nil
	}
	f, err := os.Create(fmt.Sprintf("sprites/%04d.png", idx))
	if err != nil {
		return err
	}
	defer f.Close()

	r, w := io.Pipe()

	var g errgroup.Group

	g.Go(func() error {
		defer w.Close()
		if err := png.Encode(w, subimg); err != nil {
			return err
		}
		return nil
	})

	pngr, err := pngchunks.NewReader(r)
	if err != nil {
		return err
	}

	pngw, err := pngchunks.NewWriter(f)
	if err != nil {
		return err
	}

	for {
		chunk, err := pngr.NextChunk()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
		}

		if err := pngw.WriteChunk(chunk.Length(), chunk.Type(), chunk); err != nil {
			return err
		}

		if chunk.Type() == "tRNS" {
			// Pack metadata in here.
			if len(fullPalette) > 256 {
				var buf bytes.Buffer
				buf.WriteString("extra")
				buf.WriteByte('\x00')
				buf.WriteByte('\x08')
				for _, c := range fullPalette[256:] {
					binary.Write(&buf, binary.LittleEndian, c.(color.RGBA))
					buf.WriteByte('\x00')
					buf.WriteByte('\x00')
				}
				if err := pngw.WriteChunk(int32(buf.Len()), "sPLT", bytes.NewBuffer(buf.Bytes())); err != nil {
					return err
				}
			}

			{
				var buf bytes.Buffer
				buf.WriteString("sctrl")
				buf.WriteByte('\x00')
				buf.WriteByte('\xff')
				for _, info := range infos {
					var action uint8
					switch info.Action {
					case sprites.FrameActionNext:
						action = 0
					case sprites.FrameActionLoop:
						action = 1
					case sprites.FrameActionStop:
						action = 2
					}

					binary.Write(&buf, binary.LittleEndian, struct {
						Left    int16
						Top     int16
						Right   int16
						Bottom  int16
						OriginX int16
						OriginY int16
						Delay   uint8
						Action  uint8
					}{
						int16(info.BBox.Min.X),
						int16(info.BBox.Min.Y),
						int16(info.BBox.Max.X),
						int16(info.BBox.Max.Y),
						int16(info.Origin.X),
						int16(info.Origin.Y),
						uint8(info.Delay),
						action,
					})
				}
				if err := pngw.WriteChunk(int32(buf.Len()), "zTXt", bytes.NewBuffer(buf.Bytes())); err != nil {
					return err
				}

			}
		}

		if err := chunk.Close(); err != nil {
			return err
		}
	}

	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func main() {
	flag.Parse()

	f, err := os.Open(flag.Arg(0))
	if err != nil {
		log.Fatalf("%s", err)
	}

	buf, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatalf("%s", err)
	}

	r := bytes.NewReader(buf)

	romTitle, err := gbarom.ReadROMTitle(r)
	if err != nil {
		log.Fatalf("%s", err)
	}

	log.Printf("Game title: %s", romTitle)

	romID, err := gbarom.ReadROMID(f)
	if err != nil {
		log.Fatalf("%s", err)
	}

	info := sprites.FindROMInfo(romID)
	if info == nil {
		log.Fatalf("unsupported game")
	}

	if _, err := r.Seek(info.Offset, os.SEEK_SET); err != nil {
		log.Fatalf("%s", err)
	}

	s := make([][]sprites.Animation, 0, info.Count)

	bar1 := progressbar.Default(int64(info.Count))
	bar1.Describe("decode")
	for i := 0; i < info.Count; i++ {
		bar1.Add(1)
		bar1.Describe(fmt.Sprintf("decode: %04d", i))
		anims, err := sprites.ReadNext(r)
		if err != nil {
			log.Printf("error reading %04d: %s", i, err)
			continue
		}
		s = append(s, anims)
	}

	os.Mkdir("sprites", 0o700)

	bar2 := progressbar.Default(int64(len(s)))
	bar2.Describe("dump")
	type work struct {
		idx   int
		anims []sprites.Animation
	}

	ch := make(chan work, runtime.NumCPU())

	var g errgroup.Group
	for i := 0; i < runtime.NumCPU(); i++ {
		g.Go(func() error {
			for w := range ch {
				bar2.Add(1)
				bar2.Describe(fmt.Sprintf("dump: %04d", w.idx))
				if err := processOne(w.idx, w.anims); err != nil {
					return err
				}
			}
			return nil
		})
	}

	for spriteIdx, anims := range s {
		ch <- work{spriteIdx, anims}
	}
	close(ch)

	if err := g.Wait(); err != nil {
		log.Fatalf("%s", err)
	}
}
