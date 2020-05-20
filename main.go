package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jroimartin/gocui"
	"github.com/spf13/pflag"
)

const (
	unfocusedBackground = 233
	focusedBackground   = 239
)

type Panel struct {
	view               *gocui.View
	sendInput          io.Writer
	commandInputReader io.Reader
}

var (
	maxPerLine int
	commands   []string
	shell      string
	g          *gocui.Gui
	panels     []*Panel
	logOutput  = "\n\nLog:\n"

	focusedPanel *Panel
)

func log(format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	logOutput += s + "\n"
}

func parseArgs() {
	shell = os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}

	pflag.IntVarP(&maxPerLine, "max-columns", "m", 3, "Maximum number of columns before wrapping to the next row")
	commandsFileFlag := pflag.StringP("commands-file", "f", "", "File to read commands from - each line is a command")
	pflag.StringVarP(&shell, "shell", "s", shell, "Shell to launch commands with")
	pflag.Parse()

	if *commandsFileFlag != "" {
		if pflag.NArg() != 0 {
			fmt.Println("Error: commands cannot be specified on the commandline together with --commands-file")
			pflag.Usage()
			os.Exit(1)
		}

		fileContents, err := ioutil.ReadFile(*commandsFileFlag)
		if err != nil {
			fmt.Printf("Error reading commands file: %v\n", err)
			os.Exit(1)
		}

		commandsUnfiltered := strings.Split(string(fileContents), "\n")
		commands = make([]string, 0, len(commandsUnfiltered))
		for _, command := range commandsUnfiltered {
			command = strings.Trim(command, " \t")
			if command != "" && command[0] != '#' {
				commands = append(commands, command)
			}
		}
	} else {
		commands = pflag.Args()
	}

	if len(commands) == 0 {
		pflag.Usage()
		os.Exit(1)
	}
}

func main() {
	parseArgs()

	gui, err := gocui.NewGui(gocui.Output256)
	if err != nil {
		fmt.Println(err)
		fmt.Println(logOutput)
		os.Exit(1)
	}
	g = gui

	panels = make([]*Panel, len(commands))
	g.SetManagerFunc(layout)
	handleInput()

	go runCommands()
	err = g.MainLoop()
	if err != nil {
		g.Close()
		fmt.Println(err)
		fmt.Println(logOutput)
		os.Exit(1)
	}

	g.Close()
	fmt.Println(logOutput)
}

func handleInput() {
	err := g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, func(*gocui.Gui, *gocui.View) error { return gocui.ErrQuit })
	if err != nil {
		g.Close()
		fmt.Println(err)
		fmt.Println(logOutput)
		os.Exit(1)
	}
}

func handleInputForPanel(panel *Panel) {
	viewName := panel.view.Name()
	err := g.SetKeybinding(viewName, gocui.MouseLeft, gocui.ModNone, func(*gocui.Gui, *gocui.View) error {
		g.Update(func(*gocui.Gui) error {
			g.SetCurrentView(viewName)
			return nil
		})
		return nil
	})

	if err != nil {
		g.Close()
		fmt.Println(err)
		fmt.Println(logOutput)
		os.Exit(1)
	}
}

func runCommands() {
	for index, command := range commands {
		var panel *Panel
		for {
			panel = panels[index]
			if panel != nil {
				break
			}
			log("Panel %d doesn't exist yet", index)
			time.Sleep(time.Millisecond)
		}
		go runCommand(command, panel)
	}
}

func createPanelStdout(view *gocui.View) io.Writer {
	reader, writer := io.Pipe()
	go (func() {
		buffer := make([]byte, 16*1024)
		for {
			n, err := reader.Read(buffer)
			toWrite := buffer[:n]
			if err != nil {
				if err == io.EOF {
					return
				}
				toWrite = []byte("\n\nError occured while reading from pipe:\n" + err.Error())
			}

			g.Update(func(*gocui.Gui) error {
				view.Write(toWrite)
				return nil
			})
		}
	})()
	return writer
}

func runCommand(command string, panel *Panel) {
	outputWriter := createPanelStdout(panel.view)
	g.Update(func(*gocui.Gui) error {
		panel.view.Title = command
		return nil
	})

	cmd := exec.Command(shell, "-c", command)
	log("Running command %s -c '%s'", shell, command)
	cmd.Stdout = outputWriter
	cmd.Stderr = outputWriter
	err := cmd.Run()
	if err != nil {
		fmt.Fprintf(outputWriter, "\n\nError while running command: %v", err)
		log("Error while running command %s: %v", command, err)
	} else if exitCode := cmd.ProcessState.ExitCode(); exitCode != 0 {
		fmt.Fprintf(outputWriter, "\n\nCommand exited with exit code %d", exitCode)
		log("Command exited with exit code %d", exitCode)
	} else {
		fmt.Fprintf(outputWriter, "\n\nCommand exited successfully")
	}
}

func newPanel(view *gocui.View) *Panel {
	reader, writer := io.Pipe()
	result := &Panel{
		view:               view,
		commandInputReader: reader,
		sendInput:          writer,
	}
	handleInputForPanel(result)
	return result
}

func addRow(row, y0 int, y1 int, columnCount int, width int, viewIndex *int) error {
	x0 := 0
	columnWidth := width / columnCount
	for column := 0; column < columnCount; column++ {
		var thisColumnWidth = columnWidth
		if column == columnCount-1 { // last column
			thisColumnWidth = width - (columnWidth * column)
		}
		view, err := g.SetView(strconv.FormatInt(int64(*viewIndex), 10), x0, y0, x0+thisColumnWidth-1, y1)
		if err != nil && err != gocui.ErrUnknownView {
			return fmt.Errorf("%d %e", *viewIndex, err)
		}
		view.Autoscroll = true
		panels[*viewIndex] = newPanel(view)
		x0 += columnWidth
		*viewIndex++
	}
	return nil
}

func layout(*gocui.Gui) error {
	width, height := g.Size()
	panelCount := len(commands)
	rowCount := int(math.Ceil(float64(panelCount) / float64(maxPerLine)))
	rowHeight := height / rowCount
	y0 := 0
	viewIndex := 0

	for row := 0; row < rowCount; row++ {
		var thisColumnCount, thisRowHeight = maxPerLine, rowHeight
		if row == rowCount-1 { // last row
			thisColumnCount = panelCount - (maxPerLine * row)
			rowHeight = height - (rowHeight * row)
		}

		err := addRow(row, y0, y0+thisRowHeight-1, thisColumnCount, width, &viewIndex)
		if err != nil {
			log("Error: %v", err)
			return err
		}

		y0 += rowHeight
	}

	return nil
}
