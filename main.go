package main

import (
	"embed"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/yosssi/ace"
	"golang.org/x/term"
	"golang.org/x/xerrors"

	"github.com/brandur/modulir"
	"github.com/brandur/modulir/modules/mace"
	"github.com/brandur/modulir/modules/mfile"
	"github.com/brandur/modulir/modules/mimage"
	"github.com/brandur/modulir/modules/mtemplate"
)

type Conf struct {
	Concurrency int
	GalEnv      string `env:"GAL_ENV" envDefault:"production"`
	MagickBin   string
	MozJPEGBin  string
	SourceDirs  []string
	TargetDir   string
	Verbose     bool
}

var conf Conf

func main() {
	rand.Seed(time.Now().UnixNano())
	if err := env.Parse(&conf); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing env configuration: %v\n", err)
		os.Exit(1)
	}

	rootCmd := &cobra.Command{
		Use:   "gal",
		Short: "Gal is simple image gallery",
		Long: strings.TrimSpace(`
Gal is a very simple image gallery that generates statically.`),
	}
	rootCmd.Flags().IntVar(&conf.Concurrency, "concurrency", 30,
		"Number of build jobs to run in parallel")
	rootCmd.Flags().StringVar(&conf.MagickBin, "magick-bin", "",
		"Path to ImageMagick binary (can also use MAGICK_BIN)")
	rootCmd.Flags().StringVar(&conf.MozJPEGBin, "mozjpeg-bin", "",
		"Path to MozJPEG binary (can also use MOZJPEG_BIN)")
	rootCmd.Flags().BoolVar(&conf.Verbose, "verbose", false,
		"Run in verobse mode")

	buildCmd := &cobra.Command{
		Use:   "build [path ...]",
		Short: "Run a single build loop",
		Long: strings.TrimSpace(`
Starts the build loop that watches for local changes and runs
when they're detected. A webserver is started on PORT (default
5002).`),
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			mustImageBins()
			mimage.MagickBin = conf.MagickBin
			mimage.MozJPEGBin = conf.MozJPEGBin
			conf.SourceDirs = args
			modulir.Build(getModulirConfig(), build)
		},
	}
	buildCmd.Flags().StringVarP(&conf.TargetDir, "target-dir", "t", "",
		"Path to directory where to put output artifacts (required)")
	_ = buildCmd.MarkFlagRequired("target-dir")
	rootCmd.AddCommand(buildCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Build function
//
//
//
//////////////////////////////////////////////////////////////////////////////

func build(c *modulir.Context) []error {
	c.Log.Debugf("Running build loop")

	//
	// Common directories
	//
	// Create these outside of the job system because jobs below may depend on
	// their existence.
	//

	{
		commonDirs := []string{
			c.TargetDir + "/assets",
			c.TargetDir + "/photos",
		}
		for _, dir := range commonDirs {
			err := mfile.EnsureDir(c, dir)
			if err != nil {
				return []error{nil}
			}
		}
	}

	//
	// Symlinks
	//

	{
		if conf.GalEnv != envProduction {
			commonSymlinks := [][2]string{
				{c.SourceDir + "/assets/css", c.TargetDir + "/assets/css"},
			}
			for _, link := range commonSymlinks {
				err := mfile.EnsureSymlink(c, link[0], link[1])
				if err != nil {
					return []error{nil}
				}
			}
		}
	}

	//
	// CSS / Static assets
	//

	if conf.GalEnv == envProduction {
		c.AddJob("static assets", func() (bool, error) {
			return writeStaticAssets(c, ".")
		})
	}

	//
	// Photos
	//

	var allPhotoPaths []string

	for _, dir := range conf.SourceDirs {
		dir = path.Clean(dir)
		outerDir := path.Dir(dir)
		base := path.Base(dir)
		photoPaths, err := recurseDir(c, outerDir, base)
		if err != nil {
			return []error{xerrors.Errorf("error reading dir '%s': %w", dir, err)}
		}
		allPhotoPaths = append(allPhotoPaths, photoPaths...)
	}

	//
	// Index
	//

	c.AddJob("page: index", func() (bool, error) {
		return renderIndex(c, allPhotoPaths)
	})

	//
	// Robots.txt
	//

	c.AddJob("robots.txt", func() (bool, error) {
		return renderRobotsTxt(c)
	})

	return nil
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Helpers
//
//
//
//////////////////////////////////////////////////////////////////////////////

const envProduction = "production"

//go:embed assets/css/*.css
var staticAssets embed.FS

//go:embed views/*.ace
var views embed.FS

func getLog() *logrus.Logger {
	log := logrus.New()

	if conf.Verbose {
		log.SetLevel(logrus.DebugLevel)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}

	return log
}

func getModulirConfig() *modulir.Config {
	return &modulir.Config{
		Concurrency: conf.Concurrency,
		Log:         getLog(),
		LogColor:    term.IsTerminal(int(os.Stdout.Fd())),
		Port:        12345,
		SourceDir:   ".",
		TargetDir:   conf.TargetDir,
		Websocket:   false,
	}
}

var cropDefault = &mimage.PhotoCropSettings{Portrait: "2:3", Landscape: "3:2", Square: "1:1"}

var defaultPhotoSizes = []mimage.PhotoSize{
	{Suffix: "", Width: 400, CropSettings: cropDefault},
	{Suffix: "@2x", Width: 800, CropSettings: cropDefault},
}

func fetchAndResizePhoto(c *modulir.Context, originalPath, targetDir string) (bool, error) {
	return mimage.ResizeImage(c, originalPath,
		targetDir, strings.TrimSuffix(path.Base(originalPath), path.Ext(originalPath)),
		mimage.PhotoGravityCenter, defaultPhotoSizes)
}

func mustImageBins() {
	// May have come in via arg
	if conf.MagickBin == "" {
		conf.MagickBin = os.Getenv("MAGICK_BIN")
	}
	if conf.MagickBin == "" {
		fmt.Fprintf(os.Stderr, "Must either set MAGICK_BIN or --magick-bin\n")
		os.Exit(1)
	}

	if conf.MozJPEGBin == "" {
		conf.MozJPEGBin = os.Getenv("MOZJPEG_BIN")
	}
	if conf.MozJPEGBin == "" {
		fmt.Fprintf(os.Stderr, "Must either set MOZJPEG_BIN or --mozjpeg-bin\n")
		os.Exit(1)
	}
}

func recurseDir(c *modulir.Context, basePath, dir string) ([]string, error) {
	dirPath := path.Join(basePath, dir)
	infos, err := ioutil.ReadDir(dirPath)
	if err != nil {
		return nil, xerrors.Errorf("error reading directory '%s': %w", dirPath, err)
	}

	photoPaths := make([]string, 0, len(infos))

	for _, info := range infos {
		if info.IsDir() {
			subPhotoPaths, err := recurseDir(c, basePath, path.Join(dir, info.Name()))
			if err != nil {
				return nil, err
			}
			photoPaths = append(photoPaths, subPhotoPaths...)
		}

		ext := strings.ToLower(path.Ext(info.Name()))
		if ext != ".jpg" {
			continue
		}

		inputPath := path.Join(basePath, dir, info.Name())
		outputPath := path.Join(c.TargetDir, "photos", dir)
		photoPaths = append(photoPaths, path.Join("photos", dir, url.PathEscape(info.Name())))
		c.AddJob(fmt.Sprintf("photo: %v", inputPath), func() (bool, error) {
			return fetchAndResizePhoto(c, inputPath, outputPath)
		})
	}

	return photoPaths, nil
}

func renderIndex(c *modulir.Context, allPhotoPaths []string) (bool, error) {
	// Randomize how images show up
	rand.Shuffle(len(allPhotoPaths), func(i, j int) {
		allPhotoPaths[i], allPhotoPaths[j] = allPhotoPaths[j], allPhotoPaths[i]
	})

	aceOpts := &ace.Options{FuncMap: mtemplate.FuncMap}

	// In production, use views bundled into the binary by way of `go:embed`. In
	// development, we read from local disk for less painful iteration on design
	// where views need to be changed incrementally.
	if conf.GalEnv == envProduction {
		aceOpts.Asset = views.ReadFile
	}

	// Pretty HTML
	aceOpts.Indent = "    "

	err := mace.RenderFile(c, "./views/layout.ace", "./views/index.ace",
		path.Join(c.TargetDir, "index.html"), aceOpts, map[string]interface{}{
			"AllPhotoPaths": allPhotoPaths,
		})
	if err != nil {
		return false, err
	}

	return true, nil
}

func renderRobotsTxt(c *modulir.Context) (bool, error) {
	content := `User-agent: *
Disallow: /
`

	filePath := c.TargetDir + "/robots.txt"
	outFile, err := os.Create(filePath)
	if err != nil {
		return true, xerrors.Errorf("error creating file '%s': %w", filePath, err)
	}
	if _, err := outFile.WriteString(content); err != nil {
		return true, xerrors.Errorf("error writing file '%s': %w", filePath, err)
	}
	outFile.Close()

	return true, nil
}

func writeStaticAssets(c *modulir.Context, relativePath string) (bool, error) {
	dirEntries, err := staticAssets.ReadDir(relativePath)
	if err != nil {
		return false, xerrors.Errorf("error reading static assets structure: %w", err)
	}
	for _, dirEntry := range dirEntries {
		target := path.Join(c.TargetDir, relativePath, dirEntry.Name())

		if dirEntry.IsDir() {
			if err := mfile.EnsureDir(c, target); err != nil {
				return false, err
			}
			return writeStaticAssets(c, path.Join(relativePath, dirEntry.Name()))
		}

		sourceFile, err := staticAssets.Open(path.Join(relativePath, dirEntry.Name()))
		if err != nil {
			return false, xerrors.Errorf("error opening file '%s': %w", dirEntry.Name(), err)
		}
		defer sourceFile.Close()

		targetFile, err := os.Create(target)
		if err != nil {
			return false, xerrors.Errorf("error creating file '%s': %w", target, err)
		}
		defer sourceFile.Close()

		if _, err := io.Copy(targetFile, sourceFile); err != nil {
			return false, xerrors.Errorf("error copying file '%s' to disk: %w", dirEntry.Name(), err)
		}
	}

	return true, nil
}
