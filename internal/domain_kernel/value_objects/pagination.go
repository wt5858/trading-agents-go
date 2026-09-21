package value_objects

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

// Page 是分页请求值对象，构造时即完成边界收敛。
type Page struct {
	Number int // 从 1 开始
	Size   int
}

func NewPage(number, size int) Page {
	if number < 1 {
		number = 1
	}
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return Page{Number: number, Size: size}
}

func (p Page) Offset() int { return (p.Number - 1) * p.Size }
func (p Page) Limit() int  { return p.Size }

// PageResult 是分页查询结果。
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

func NewPageResult[T any](items []T, total int64, p Page) PageResult[T] {
	if items == nil {
		items = []T{}
	}
	return PageResult[T]{Items: items, Total: total, Page: p.Number, PageSize: p.Size}
}
