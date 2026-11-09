# SwiiftXML

SwiiftXML opens XML files that are too large for normal editors (many gigabytes). You
go through the file one row at a time, and each row is shown as a tree you can open
step by step.

- **Nothing to install.** It is a single program file. Double-click it and it opens in your web browser.
- **Your data stays on your computer.** SwiiftXML only talks to your own browser. It does not
  use the internet, does not send data anywhere and does not change your files.
- **Fast with huge files.** The file is never loaded into memory. You can start browsing right
  away, while SwiiftXML is still indexing the rest of the file in the background.

## Starting SwiiftXML

| Computer | File to use |
|---|---|
| Windows | `SwiiftXML.exe` |
| Linux | `SwiiftXML-linux` (run `chmod +x SwiiftXML-linux` once if it does not start) |
| Linux on ARM (Chromebook, Raspberry Pi) | `SwiiftXML-linux-arm64` |

The first two are for ordinary 64-bit Intel/AMD computers. Copy the file anywhere, for example
to the desktop or a shared folder. Nothing else is needed.

1. Double-click the program. A small black window opens, and then SwiiftXML opens in your browser.
2. Choose the XML file:
   - paste the file's path in the box at the top. In Windows Explorer, hold **Shift**,
     right-click the file and choose **Copy as path**. Or:
   - click your way to the file in the folder list. Or:
   - drag the XML file onto the SwiiftXML program icon before starting it.
3. When you are done, close the black window or click **⏻** in the top-right corner.

If the browser does not open by itself, copy the address shown in the black window into
your browser.

**First start on Windows:** if *"Windows protected your PC"* appears, click **More info → Run anyway**.

## Using it

**Rows.** SwiiftXML works out by itself which element repeats in the file and uses it as the
row, for example every `<AssistantTime>` or `<Order>`. You can choose another element in the
**Rows** menu at the top.

**Tree.** The selected row is shown as a tree on the right. Values are shown next to their
element names. Click **▸** (or press **→**) to open a node. When you move to the next row, the
same parts of the tree stay open and the same field stays selected, so you can compare rows quickly.

**Details.** The bottom panel shows the selected node's full value, its attributes and its
place in the row. From there you can view the node's XML, copy it, or save it as a separate file.

**Summarize.** Right-click a field in the tree, for example `Duration`, and choose **Sum of
Duration…** or **Count of Duration…**. In the window that opens, click **＋ Group by a field**,
pick for example `ClientNumber`, and click **Run**. SwiiftXML reads all rows in the file and
shows one line per client with the total hours, number of rows, average, minimum and maximum.
To combine fields, click **＋ Add field** again (up to 5), for example `ClientNumber` +
`EmployeeNumber` for the hours of each employee per client; **✕** removes a field. Right-clicking
a field and choosing **Also group by…** adds it to the grouping as well. Click a
column header to sort. **Copy table** pastes straight into Excel, and **Save as CSV** writes a
file that opens in Excel with Swedish decimal commas. You can also right-click a field and choose
**Group by…** first. If something cannot be summarized, for example summing a text field or
a field that contains other fields, an error box explains why and offers to count instead.

**Search** (`Ctrl+F`) looks through the whole file and jumps to the matching row and field.
Press **Enter** again for the next match. Search looks in the raw XML, so you can also
search for tags like `<Status>`.

| Keys | Action |
|---|---|
| `N` / `P` (or `↓` `↑` in the row list) | Next / previous row |
| `G` | Go to a row number |
| `Tab` | Switch between the row list and the tree |
| `↑` `↓` in the tree | Move node by node |
| `→` / `←` | Open / close a node (or go to child / parent) |
| `*` / `-` | Expand everything below the node / collapse all |
| `X` | Show the XML of the selected node |
| Right-click, or `Shift+F10` | Sum, Count or Group by the selected field |
| `Ctrl+F`, `Enter` / `F3` | Search, next match |
| `?` | Show all shortcuts |

## Building from source

You need [Go](https://go.dev/dl/) 1.27 or newer. Nothing else is needed: the user interface
is plain HTML/CSS/JavaScript and is built into the program.

```sh
./build.sh          # Windows, Linux and Linux-ARM builds in dist/, plus SHA256SUMS.txt
build.bat           # on Windows: builds SwiiftXML.exe
go test ./...       # run the tests
```

For other computers, build with `GOOS`/`GOARCH` set, e.g.
`GOOS=darwin GOARCH=arm64 go build -o SwiiftXML-mac .` for Macs with Apple chips.

Command-line options: `SwiiftXML [-port 8080] [-no-browser] [file.xml]`.

## How it works

- `internal/xmlview/scanner.go` is a small, fast XML tokenizer that reads the file in chunks
  and only finds where tags start and end.
- When a file is opened, one background pass records where the rows start. It stores a
  checkpoint every 32 rows or every 256 KB, for the first four nesting levels and for each
  element name on them. To show row *N*, SwiiftXML jumps to the nearest checkpoint and reads
  only a few kilobytes from there.
- Each row's tree is parsed only when you open it. Elements with very many children are
  loaded in pages ("Load more"), and long values are fetched in full only when selected.
- `server.go` serves the user interface (`web/`) on `127.0.0.1`. Every request has to carry a
  random key from the start-up address, so other web pages cannot read from it.

Limits: UTF-8, ISO-8859-1 and Windows-1252 files are supported. UTF-16 files are not.
Indexing runs at roughly 150–250 MB/s, depending on the disk.
