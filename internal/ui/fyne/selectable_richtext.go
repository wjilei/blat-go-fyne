// SelectableRichText 是带文本选择能力的彩色日志组件。
//
// 背景：Fyne v2.8 的 RichText 不支持选择（RichTextSegment.Select 为 no-op，
// 官方 issue #21/#5946），而需求是"日志按级别着色 + 鼠标选择复制"。实现
// 照抄官方 Label.Selectable 的组合模式（widget/label.go + widget/selectable.go）：
// 内部用 widget.RichText 渲染彩色文本，外壳实现鼠标拖选/双击选词/右键
// 复制菜单/Ctrl+C，选择高亮用 canvas.Rectangle 叠加在文本下层。
//
// 官方 selectable.go 依赖 RichText 的 row/rows/rowBoundary/charMinSize/
// lineSizeToColumn（全部 unexported，外部包无法调用），这里改为自维护
// 行数据：AppendSegment 追加文本时同步拆行记录（rows 每行字符、rowOffsets
// 每行在全文 String() 中的 rune 偏移、totalLen 全文长度），坐标计算全部
// 基于行数据 + fyne.MeasureText，与官方公式保持一致（文本渲染起点 x =
// innerPad，行高 = "M" 字符高）。
//
// 注意：Wrapping 必须为 TextWrapOff——WordWrap 时 RichText 按视觉行布局，
// 而行数据是逻辑行，坐标换算会错位导致选择错乱；滚动交给外层容器
// （container.NewScroll 双向），长行可横向滚动。
package fyneui

import (
	"math"
	"strings"
	"time"
	"unicode"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// SelectableRichText 是彩色日志展示组件：可选择（拖选/双击选词）并复制。
type SelectableRichText struct {
	widget.BaseWidget
	provider *widget.RichText

	rows       [][]rune // 每行字符（不含换行）
	rowOffsets []int    // 每行首个字符在 String() 中的 rune 偏移
	totalLen   int      // String() 全文 rune 数（行间含换行符）

	cursorRow, cursorColumn          int
	selectRow, selectColumn          int // 选择起点；选择区间 = 起点 → 光标位置
	focussed, selecting, selectEnded bool
	doubleTappedAtUnixMillis         int64

	theme fyne.Theme
	style fyne.TextStyle
}

// NewSelectableRichText 构造彩色可选择日志组件。
func NewSelectableRichText() *SelectableRichText {
	t := &SelectableRichText{}
	t.ExtendBaseWidget(t)
	t.provider = widget.NewRichText()
	// 关键：TextWrapOff 时行数据 = 逻辑行 = 视觉行，坐标换算才准确；
	// WordWrap 会按宽度换行产生视觉行，选择会错乱。
	t.provider.Wrapping = fyne.TextWrapOff
	t.provider.Scroll = container.ScrollNone // 滚动交给外层容器
	t.provider.Truncation = fyne.TextTruncateOff
	t.theme = fyne.CurrentApp().Settings().Theme()
	return t
}

// AppendSegment 追加一段彩色文本（含换行时拆成多行），并刷新显示。
// 行条目不含尾随换行符（TextSegment 的 \n 只负责视觉换行），只有行与行
// 之间计入 +1 偏移；连续多次调用（每段一行）时偏移依然自洽。
func (t *SelectableRichText) AppendSegment(colorName fyne.ThemeColorName, text string) {
	t.provider.Segments = append(t.provider.Segments, &widget.TextSegment{
		Style: widget.RichTextStyle{ColorName: colorName, SizeName: theme.SizeNameText},
		Text:  text,
	})
	parts := strings.Split(text, "\n")
	if last := len(parts) - 1; last >= 0 && parts[last] == "" {
		parts = parts[:last] // 尾部换行不产生空行条目
	}
	for i, ln := range parts {
		r := []rune(ln)
		t.rows = append(t.rows, r)
		t.rowOffsets = append(t.rowOffsets, t.totalLen)
		t.totalLen += len(r)
		if i < len(parts)-1 {
			t.totalLen++ // 行间换行符
		}
	}
	t.Refresh()
}

// Clear 清空全部内容与选择状态。
func (t *SelectableRichText) Clear() {
	t.provider.Segments = nil
	t.rows = nil
	t.rowOffsets = nil
	t.totalLen = 0
	t.selecting = false
	t.selectRow, t.selectColumn = 0, 0
	t.cursorRow, t.cursorColumn = 0, 0
	t.Refresh()
}

// String 返回全文（行间用换行符连接），与 rowOffsets 的偏移一致。
func (t *SelectableRichText) String() string {
	var b strings.Builder
	for i, r := range t.rows {
		if i > 0 {
			b.WriteRune('\n')
		}
		b.WriteString(string(r))
	}
	return b.String()
}

func (t *SelectableRichText) CreateRenderer() fyne.WidgetRenderer {
	return &selectableRichTextRenderer{t: t}
}

// ---- fyne.Focusable ----

func (t *SelectableRichText) FocusGained() {
	t.focussed = true
	t.Refresh()
}

func (t *SelectableRichText) FocusLost() {
	t.focussed = false
	t.Refresh()
}

func (t *SelectableRichText) TypedKey(*fyne.KeyEvent) {}
func (t *SelectableRichText) TypedRune(rune)          {}

// ---- desktop.Cursorable / desktop.Mouseable / desktop.DoubleTappable ----

func (t *SelectableRichText) Cursor() desktop.Cursor {
	return desktop.TextCursor
}

func (t *SelectableRichText) DoubleTapped(p *fyne.PointEvent) {
	t.doubleTappedAtUnixMillis = time.Now().UnixMilli()
	t.updateMousePointer(p.Position)
	if t.cursorRow < 0 || t.cursorRow >= len(t.rows) {
		return
	}
	row := t.rows[t.cursorRow]
	start, end := getTextWhitespaceRegion(row, t.cursorColumn, false)
	if start == -1 || end == -1 {
		return
	}
	t.selectRow = t.cursorRow
	t.selectColumn = start
	t.cursorColumn = end
	t.selecting = true
	t.grabFocus()
	t.Refresh()
}

func (t *SelectableRichText) MouseDown(m *desktop.MouseEvent) {
	if isTripleTap(t.doubleTappedAtUnixMillis, time.Now().UnixMilli()) {
		t.selectCurrentRow(false)
		return
	}
	t.grabFocus()
	if t.selecting && m.Button == desktop.MouseButtonPrimary {
		t.selecting = false
	}
}

func (t *SelectableRichText) MouseUp(ev *desktop.MouseEvent) {
	if ev.Button == desktop.MouseButtonSecondary {
		return
	}
	start, _ := t.selection()
	if (start == -1 || (t.selectRow == t.cursorRow && t.selectColumn == t.cursorColumn)) && t.selecting {
		t.selecting = false
	}
	t.Refresh()
}

// ---- fyne.Draggable ----

func (t *SelectableRichText) DragEnd() {
	if t.cursorColumn == t.selectColumn && t.cursorRow == t.selectRow {
		t.selecting = false
	}
	if !t.selecting {
		t.Refresh()
	}
	t.selectEnded = true
}

func (t *SelectableRichText) Dragged(d *fyne.DragEvent) {
	if !t.selecting || t.selectEnded {
		t.selectEnded = false
		t.updateMousePointer(d.Position)
		startPos := d.Position.Subtract(d.Dragged)
		t.selectRow, t.selectColumn = t.getRowCol(startPos)
		t.selecting = true
		t.grabFocus()
	}
	t.updateMousePointer(d.Position)
	t.Refresh()
}

// ---- fyne.Shortcutable / fyne.SecondaryTappable ----

// TypedShortcut 只放行复制（Ctrl+C），屏蔽粘贴等编辑快捷方式。
func (t *SelectableRichText) TypedShortcut(sh fyne.Shortcut) {
	switch sh.(type) {
	case *fyne.ShortcutCopy:
		fyne.CurrentApp().Clipboard().SetContent(t.SelectedText())
	}
}

// TappedSecondary 右键弹出"复制"菜单（无选择时复制空串，无害）。
func (t *SelectableRichText) TappedSecondary(ev *fyne.PointEvent) {
	app := fyne.CurrentApp()
	c := app.Driver().CanvasForObject(t)
	if c == nil {
		return
	}
	m := fyne.NewMenu("", fyne.NewMenuItem("复制", func() {
		app.Clipboard().SetContent(t.SelectedText())
	}))
	widget.ShowPopUpMenuAtPosition(m, c, ev.AbsolutePosition)
}

// ---- 选择逻辑（照抄官方 selectable.go，provider 依赖换成自维护行数据）----

// SelectedText 返回当前选中的文本；无选择返回空串。
func (t *SelectableRichText) SelectedText() string {
	if t == nil || !t.selecting {
		return ""
	}
	start, stop := t.selection()
	if start == stop {
		return ""
	}
	r := []rune(t.String())
	if start < 0 || stop > len(r) || start > stop {
		return ""
	}
	return string(r[start:stop])
}

func (t *SelectableRichText) cursorColAt(text []rune, pos fyne.Position) int {
	th := t.theme
	textSize := th.Size(t.getSizeName())
	innerPad := th.Size(theme.SizeNameInnerPadding)

	for i := 0; i < len(text); i++ {
		str := string(text[0:i])
		wid := fyne.MeasureText(str, textSize, t.style).Width
		charWid := fyne.MeasureText(string(text[i]), textSize, t.style).Width
		if pos.X < innerPad+wid+(charWid/2) {
			return i
		}
	}
	return len(text)
}

func (t *SelectableRichText) getRowCol(p fyne.Position) (int, int) {
	if len(t.rows) == 0 {
		return 0, 0
	}
	th := t.theme
	innerPad := th.Size(theme.SizeNameInnerPadding)
	rowHeight := t.rowHeight()

	row := int(math.Floor(float64(p.Y-innerPad+th.Size(theme.SizeNameLineSpacing)) / float64(rowHeight)))
	col := 0
	if row < 0 {
		row = 0
	} else if row >= len(t.rows) {
		row = len(t.rows) - 1
		col = len(t.rows[row])
	} else {
		col = t.cursorColAt(t.rows[row], p)
	}
	return row, col
}

// selectCurrentRow 选中光标所在整行。
func (t *SelectableRichText) selectCurrentRow(bool) {
	t.grabFocus()
	t.selectRow = t.cursorRow
	t.selectColumn = 0
	t.cursorColumn = len(t.rows[t.cursorRow])
	t.Refresh()
}

// selection 返回选中区间在全文中的 [start, stop) rune 偏移。
func (t *SelectableRichText) selection() (int, int) {
	noSelection := !t.selecting || (t.cursorRow == t.selectRow && t.cursorColumn == t.selectColumn)
	if noSelection {
		return -1, -1
	}
	rowA, colA := t.cursorRow, t.cursorColumn
	rowB, colB := t.selectRow, t.selectColumn
	if rowA > t.selectRow || (rowA == t.selectRow && colA > t.selectColumn) {
		rowA, colA = t.selectRow, t.selectColumn
		rowB, colB = t.cursorRow, t.cursorColumn
	}
	return textPosFromRowCol(rowA, colA, t.rowOffsets), textPosFromRowCol(rowB, colB, t.rowOffsets)
}

func textPosFromRowCol(row, col int, rowOffsets []int) int {
	if row < 0 || row >= len(rowOffsets) {
		return col
	}
	return rowOffsets[row] + col
}

func (t *SelectableRichText) updateMousePointer(p fyne.Position) {
	row, col := t.getRowCol(p)
	t.cursorRow, t.cursorColumn = row, col
	if !t.selecting {
		t.selectRow, t.selectColumn = row, col
	}
}

func (t *SelectableRichText) getSizeName() fyne.ThemeSizeName {
	return theme.SizeNameText
}

// rowHeight 返回单行文本高度（与官方 charMinSize 等价）。
func (t *SelectableRichText) rowHeight() float32 {
	return fyne.MeasureText("M", t.theme.Size(t.getSizeName()), t.style).Height
}

func (t *SelectableRichText) grabFocus() {
	if c := fyne.CurrentApp().Driver().CanvasForObject(t); c != nil {
		c.Focus(t)
	}
}

func isTripleTap(double, nowMilli int64) bool {
	return nowMilli-double <= fyne.CurrentApp().Driver().DoubleTapDelay().Milliseconds()
}

// ---- 渲染器：选择高亮矩形层（底）+ 彩色文本层（顶）----

type selectableRichTextRenderer struct {
	t          *SelectableRichText
	selections []fyne.CanvasObject
}

func (r *selectableRichTextRenderer) Destroy() {}

func (r *selectableRichTextRenderer) Layout(size fyne.Size) {
	r.t.provider.Resize(size)
}

func (r *selectableRichTextRenderer) MinSize() fyne.Size {
	return r.t.provider.MinSize()
}

func (r *selectableRichTextRenderer) Objects() []fyne.CanvasObject {
	objs := make([]fyne.CanvasObject, 0, len(r.selections)+1)
	objs = append(objs, r.selections...)
	objs = append(objs, r.t.provider)
	return objs
}

func (r *selectableRichTextRenderer) Refresh() {
	// 必须先刷新 provider：AppendSegment/Clear 只改 Segments，provider 的
	// rowBounds/minCache 需要 Refresh 才重算，否则渲染的仍是旧内容（空白）。
	// 对齐官方 labelRenderer.Refresh 首行 r.l.provider.Refresh()。
	r.t.provider.Refresh()
	r.buildSelection()
	v := fyne.CurrentApp().Settings().ThemeVariant()
	selectionColor := r.t.theme.Color(theme.ColorNameSelection, v)
	for _, s := range r.selections {
		rect := s.(*canvas.Rectangle)
		rect.FillColor = selectionColor
		if r.t.focussed {
			rect.Show()
		} else {
			rect.Hide()
		}
	}
	canvas.Refresh(r.t)
}

// buildSelection 为选中区间逐行生成高亮矩形（照抄官方 selectableRenderer，
// 坐标换算用自维护行数据）。
func (r *selectableRichTextRenderer) buildSelection() {
	th := r.t.theme
	textSize := th.Size(r.t.getSizeName())
	v := fyne.CurrentApp().Settings().ThemeVariant()

	cursorRow, cursorCol := r.t.cursorRow, r.t.cursorColumn
	selectRow, selectCol := -1, -1
	if r.t.selecting {
		selectRow, selectCol = r.t.selectRow, r.t.selectColumn
	}
	if selectRow == -1 || (cursorRow == selectRow && cursorCol == selectCol) {
		r.selections = r.selections[:0]
		return
	}

	innerPad := th.Size(theme.SizeNameInnerPadding)
	// 列/行 → 绘制坐标。文本渲染起点 x = innerPad（对齐官方 lineSizeToColumn
	// 的 total.Add(innerPad - inset)）。
	getCoordinates := func(column, row int) (float32, float32) {
		x := innerPad
		if column > 0 && row >= 0 && row < len(r.t.rows) {
			runes := r.t.rows[row]
			if column > len(runes) {
				column = len(runes)
			}
			x += fyne.MeasureText(string(runes[:column]), textSize, r.t.style).Width
		}
		y := float32(row)*r.t.rowHeight() - th.Size(theme.SizeNameInputBorder) + innerPad
		return x, y
	}

	lineHeight := r.t.rowHeight()

	minmax := func(a, b int) (int, int) {
		if a < b {
			return a, b
		}
		return b, a
	}

	selectStartRow, selectEndRow := minmax(selectRow, cursorRow)
	selectStartCol, selectEndCol := minmax(selectCol, cursorCol)
	if selectRow < cursorRow {
		selectStartCol, selectEndCol = selectCol, cursorCol
	}
	if selectRow > cursorRow {
		selectStartCol, selectEndCol = cursorCol, selectCol
	}
	rowCount := selectEndRow - selectStartRow + 1

	if len(r.selections) > rowCount {
		r.selections = r.selections[:rowCount]
	}
	for i := 0; i < rowCount; i++ {
		if len(r.selections) <= i {
			box := canvas.NewRectangle(th.Color(theme.ColorNameSelection, v))
			r.selections = append(r.selections, box)
		}
		row := selectStartRow + i
		startCol, endCol := selectStartCol, selectEndCol
		if selectStartRow < row {
			startCol = 0
		}
		if selectEndRow > row {
			endCol = len(r.t.rows[row])
		}

		x1, y1 := getCoordinates(startCol, row)
		x2, _ := getCoordinates(endCol, row)
		r.selections[i].Resize(fyne.NewSize(x2-x1+1, lineHeight))
		r.selections[i].Move(fyne.NewPos(x1-1, y1))
	}
}

// ---- 词边界工具（照抄官方 entry.go，供双击选词）----

// getTextWhitespaceRegion 返回从 col 出发、向两侧扩展到词边界的区间
// [start, end)；行首/行尾空白视为可扩展（expand=true 时）。空行返回 (-1,-1)。
func getTextWhitespaceRegion(row []rune, col int, expand bool) (int, int) {
	if len(row) == 0 || col < 0 {
		return -1, -1
	}
	if col >= len(row) {
		col = len(row) - 1
	}

	// 把行文本映射成空格（词分隔符）与短横（词字符）串，再找边界。
	space := func(r rune) rune {
		if isWordSeparator(r) {
			return ' '
		}
		return '-'
	}
	toks := strings.Map(space, string(row))
	c := byte(' ')

	startCheck := col
	endCheck := col
	if expand {
		if col > 0 && toks[col-1] == ' ' { // 忽略前导空白再计数
			startCheck = strings.LastIndexByte(toks[:startCheck], '-')
			if startCheck == -1 {
				startCheck = 0
			}
		}
		if toks[col] == ' ' { // 忽略当前空白再计数
			endCheck = col + strings.IndexByte(toks[endCheck:], '-')
		}
	} else if toks[col] == ' ' {
		c = byte('-')
	}

	start := strings.LastIndexByte(toks[:startCheck], c) + 1
	end := -1
	if endCheck != -1 {
		end = strings.IndexByte(toks[endCheck:], c)
	}
	if end == -1 {
		end = len(toks)
	} else {
		end += endCheck
	}
	return start, end
}

// isWordSeparator 判断 rune 是否为词分隔符（空白或常见标点）。
func isWordSeparator(r rune) bool {
	return unicode.IsSpace(r) ||
		strings.ContainsRune("`~!@#$%^&*()-=+[{]}\\|;:'\",.<>/?", r)
}
