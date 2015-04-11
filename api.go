package api2go

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/univedo/api2go/jsonapi"
)

// The CRUD interface MUST be implemented in order to use the api2go api.
type CRUD interface {
	// FindOne returns an object by its ID
	FindOne(ID string, req Request) (interface{}, error)

	// Create a new object and return its ID
	Create(obj interface{}, req Request) (string, error)

	// Delete an object
	Delete(id string, req Request) error

	// Update an object
	Update(obj interface{}, req Request) error
}

// The FindAll interface can be optionally implemented to fetch all records at once.
type FindAll interface {
	// FindAll returns all objects
	FindAll(req Request) (interface{}, error)
}

// The FindMultiple interface can be optionally implemented to fetch multiple records by their ID at once.
type FindMultiple interface {
	// FindMultiple returns all objects for the specified IDs
	FindMultiple(IDs []string, req Request) (interface{}, error)
}

// The PaginatedFindAll interface can be optionally implemented to fetch a subset of all records.
// Pagination query parameters must be used to limit the result. Pagination URLs will automatically
// be generated by the api. You can use a combination of the following 2 query parameters:
// page[number] AND page[size]
// OR page[offset] AND page[limit]
type PaginatedFindAll interface {
	PaginatedFindAll(req Request) (obj interface{}, totalCount uint, err error)
}

type paginationQueryParams struct {
	number, size, offset, limit string
}

func newPaginationQueryParams(r *http.Request) paginationQueryParams {
	var result paginationQueryParams

	queryParams := r.URL.Query()
	result.number = queryParams.Get("page[number]")
	result.size = queryParams.Get("page[size]")
	result.offset = queryParams.Get("page[offset]")
	result.limit = queryParams.Get("page[limit]")

	return result
}

func (p paginationQueryParams) isValid() bool {
	if p.number == "" && p.size == "" && p.offset == "" && p.limit == "" {
		return false
	}

	if p.number != "" && p.size != "" && p.offset == "" && p.limit == "" {
		return true
	}

	if p.number == "" && p.size == "" && p.offset != "" && p.limit != "" {
		return true
	}

	return false
}

func (p paginationQueryParams) getLinks(r *http.Request, count uint, info information) (result map[string]string, err error) {
	result = make(map[string]string)

	params := r.URL.Query()
	prefix := ""
	baseURL := info.GetBaseURL()
	if baseURL != "" {
		prefix = baseURL
	}
	requestURL := fmt.Sprintf("%s%s", prefix, r.URL.Path)

	if p.number != "" {
		// we have number & size params
		var number uint64
		number, err = strconv.ParseUint(p.number, 10, 64)
		if err != nil {
			return
		}

		if p.number != "1" {
			params.Set("page[number]", "1")
			result["first"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())

			params.Set("page[number]", strconv.FormatUint(number-1, 10))
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())
		}

		// calculate last page number
		var size uint64
		size, err = strconv.ParseUint(p.size, 10, 64)
		if err != nil {
			return
		}
		totalPages := (uint64(count) / size)
		if (uint64(count) % size) != 0 {
			// there is one more page with some len(items) < size
			totalPages++
		}

		if number != totalPages {
			params.Set("page[number]", strconv.FormatUint(number+1, 10))
			result["next"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())

			params.Set("page[number]", strconv.FormatUint(totalPages, 10))
			result["last"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())
		}
	} else {
		// we have offset & limit params
		var offset, limit uint64
		offset, err = strconv.ParseUint(p.offset, 10, 64)
		if err != nil {
			return
		}
		limit, err = strconv.ParseUint(p.limit, 10, 64)
		if err != nil {
			return
		}

		if p.offset != "0" {
			params.Set("page[offset]", "0")
			result["first"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())

			var prevOffset uint64
			if limit > offset {
				prevOffset = 0
			} else {
				prevOffset = offset - limit
			}
			params.Set("page[offset]", strconv.FormatUint(prevOffset, 10))
			result["prev"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())
		}

		// check if there are more entries to be loaded
		if (offset + limit) < uint64(count) {
			params.Set("page[offset]", strconv.FormatUint(offset+limit, 10))
			result["next"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())

			params.Set("page[offset]", strconv.FormatUint(uint64(count)-limit, 10))
			result["last"] = fmt.Sprintf("%s?%s", requestURL, params.Encode())
		}
	}

	return
}

// API is a REST JSONAPI.
type API struct {
	router *httprouter.Router
	// Route prefix, including slashes
	prefix    string
	info      information
	resources []resource
}

type information struct {
	prefix  string
	baseURL string
}

func (i information) GetBaseURL() string {
	return i.baseURL
}

func (i information) GetPrefix() string {
	return i.prefix
}

// NewAPI returns an initialized API instance
// `prefix` is added in front of all endpoints.
func NewAPI(prefix string) *API {
	// Add initial and trailing slash to prefix
	prefixSlashes := strings.Trim(prefix, "/")
	if len(prefixSlashes) > 0 {
		prefixSlashes = "/" + prefixSlashes + "/"
	} else {
		prefixSlashes = "/"
	}

	return &API{
		router: httprouter.New(),
		prefix: prefixSlashes,
		info:   information{prefix: prefix},
	}
}

// NewAPIWithBaseURL does the same as NewAPI with the addition of
// a baseURL which get's added in front of all generated URLs.
// For example http://localhost/v1/myResource/abc instead of /v1/myResource/abc
func NewAPIWithBaseURL(prefix string, baseURL string) *API {
	api := NewAPI(prefix)
	api.info.baseURL = baseURL

	return api
}

//SetRedirectTrailingSlash enables 307 redirects on urls ending with /
//when disabled, an URL ending with / will 404
func (api *API) SetRedirectTrailingSlash(enabled bool) {
	if api.router == nil {
		panic("router must not be nil")
	}

	api.router.RedirectTrailingSlash = enabled
}

// Request holds additional information for FindOne and Find Requests
type Request struct {
	PlainRequest *http.Request
	QueryParams  map[string][]string
	Header       http.Header
}

type resource struct {
	resourceType reflect.Type
	source       CRUD
	name         string
}

func (api *API) addResource(prototype interface{}, source CRUD) *resource {
	resourceType := reflect.TypeOf(prototype)
	if resourceType.Kind() != reflect.Struct {
		panic("pass an empty resource struct to AddResource!")
	}

	name := jsonapi.Jsonify(jsonapi.Pluralize(resourceType.Name()))
	res := resource{
		resourceType: resourceType,
		name:         name,
		source:       source,
	}

	api.router.Handle("OPTIONS", api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,POST,PATCH,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.Handle("OPTIONS", api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Allow", "GET,PATCH,DELETE,OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	})

	api.router.GET(api.prefix+name, func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		err := res.handleIndex(w, r, api.info)
		if err != nil {
			handleError(err, w)
		}
	})

	api.router.GET(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleRead(w, r, ps, api.info)
		if err != nil {
			handleError(err, w)
		}
	})

	api.router.GET(api.prefix+name+"/:id/:linked", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleLinked(api, w, r, ps, api.info)
		if err != nil {
			handleError(err, w)
		}
	})

	api.router.POST(api.prefix+name, func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleCreate(w, r, api.prefix, api.info)
		if err != nil {
			handleError(err, w)
		}
	})

	api.router.DELETE(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleDelete(w, r, ps)
		if err != nil {
			handleError(err, w)
		}
	})

	api.router.PATCH(api.prefix+name+"/:id", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		err := res.handleUpdate(w, r, ps)
		if err != nil {
			handleError(err, w)
		}
	})

	api.resources = append(api.resources, res)

	return &res
}

// AddResource registers a data source for the given resource
// At least the CRUD interface must be implemented, all the other interfaces are optional.
// `resource` should by an empty struct instance such as `Post{}`. The same type will be used for constructing new elements.
func (api *API) AddResource(prototype interface{}, source CRUD) {
	api.addResource(prototype, source)
}

func buildRequest(r *http.Request) Request {
	req := Request{PlainRequest: r}
	params := make(map[string][]string)
	for key, values := range r.URL.Query() {
		params[key] = strings.Split(values[0], ",")
	}
	req.QueryParams = params
	req.Header = r.Header
	return req
}

func (res *resource) handleIndex(w http.ResponseWriter, r *http.Request, info information) error {
	var (
		objs interface{}
		err  error
	)

	pagination := newPaginationQueryParams(r)
	if pagination.isValid() {
		source, ok := res.source.(PaginatedFindAll)
		if !ok {
			return NewHTTPError(nil, "Resource does not implement the PaginatedFindAll interface", http.StatusNotFound)
		}

		var count uint
		objs, count, err = source.PaginatedFindAll(buildRequest(r))
		if err != nil {
			return err
		}

		paginationLinks, err := pagination.getLinks(r, count, info)
		if err != nil {
			return err
		}

		return respondWithPagination(objs, info, http.StatusOK, paginationLinks, w)
	}
	source, ok := res.source.(FindAll)
	if !ok {
		return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
	}

	objs, err = source.FindAll(buildRequest(r))
	if err != nil {
		return err
	}

	return respondWith(objs, info, http.StatusOK, w)
}

func (res *resource) handleRead(w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information) error {
	ids := strings.Split(ps.ByName("id"), ",")

	var (
		obj interface{}
		err error
	)

	if len(ids) == 1 {
		obj, err = res.source.FindOne(ids[0], buildRequest(r))
	} else {
		source, ok := res.source.(FindMultiple)
		if !ok {
			return NewHTTPError(nil, "Resource does not implement the FindMultiple interface", http.StatusNotFound)
		}
		obj, err = source.FindMultiple(ids, buildRequest(r))
	}

	if err != nil {
		return err
	}

	return respondWith(obj, info, http.StatusOK, w)
}

// try to find the referenced resource and call the findAll Method with referencing resource id as param
func (res *resource) handleLinked(api *API, w http.ResponseWriter, r *http.Request, ps httprouter.Params, info information) error {
	id := ps.ByName("id")
	linked := ps.ByName("linked")
	// Iterate over all struct fields and determine the type of linked
	for i := 0; i < res.resourceType.NumField(); i++ {
		field := res.resourceType.Field(i)
		fieldName := jsonapi.Jsonify(field.Name)
		kind := field.Type.Kind()
		if (kind == reflect.Ptr || kind == reflect.Slice) && fieldName == linked {
			// Check if there is a resource for this type
			fieldType := jsonapi.Pluralize(jsonapi.Jsonify(field.Type.Elem().Name()))
			for _, resource := range api.resources {
				if resource.name == fieldType {
					source, ok := resource.source.(FindAll)
					if !ok {
						return NewHTTPError(nil, "Resource does not implement the FindAll interface", http.StatusNotFound)
					}

					request := buildRequest(r)
					request.QueryParams[res.name+"ID"] = []string{id}
					obj, err := source.FindAll(request)
					if err != nil {
						return err
					}
					return respondWith(obj, info, http.StatusOK, w)
				}
			}
		}
	}

	err := Error{
		Status: string(http.StatusNotFound),
		Title:  "Not Found",
		Detail: "No resource handler is registered to handle the linked resource " + linked,
	}
	return respondWith(err, info, http.StatusNotFound, w)
}

func (res *resource) handleCreate(w http.ResponseWriter, r *http.Request, prefix string, info information) error {
	ctx, err := unmarshalJSONRequest(r)
	if err != nil {
		return err
	}
	newObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 0, 0)

	err = jsonapi.UnmarshalInto(ctx, res.resourceType, &newObjs)
	if err != nil {
		return err
	}
	if newObjs.Len() != 1 {
		return errors.New("expected one object in POST")
	}

	//TODO create multiple objects not only one.
	newObj := newObjs.Index(0).Interface()

	checkID, ok := newObj.(jsonapi.MarshalIdentifier)
	if ok {
		if checkID.GetID() != "" {
			err := Error{
				Status: string(http.StatusForbidden),
				Title:  "Forbidden",
				Detail: "Client generated IDs are not supported.",
			}

			return respondWith(err, info, http.StatusForbidden, w)
		}
	}

	id, err := res.source.Create(newObj, buildRequest(r))
	if err != nil {
		return err
	}
	w.Header().Set("Location", prefix+res.name+"/"+id)

	obj, err := res.source.FindOne(id, buildRequest(r))
	if err != nil {
		return err
	}

	return respondWith(obj, info, http.StatusCreated, w)
}

func (res *resource) handleUpdate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	obj, err := res.source.FindOne(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}

	ctx, err := unmarshalJSONRequest(r)
	if err != nil {
		return err
	}

	data, ok := ctx["data"]

	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory data key.",
			http.StatusForbidden,
		)
	}

	check, ok := data.(map[string]interface{})
	if !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"data must contain an object.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["id"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory id key.",
			http.StatusForbidden,
		)
	}

	if _, ok := check["type"]; !ok {
		return NewHTTPError(
			errors.New("Forbidden"),
			"missing mandatory type key.",
			http.StatusForbidden,
		)
	}

	updatingObjs := reflect.MakeSlice(reflect.SliceOf(res.resourceType), 1, 1)
	updatingObjs.Index(0).Set(reflect.ValueOf(obj))

	err = jsonapi.UnmarshalInto(ctx, res.resourceType, &updatingObjs)
	if err != nil {
		return err
	}
	if updatingObjs.Len() != 1 {
		return errors.New("expected one object")
	}

	updatingObj := updatingObjs.Index(0).Interface()

	if err := res.source.Update(updatingObj, buildRequest(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (res *resource) handleDelete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	err := res.source.Delete(ps.ByName("id"), buildRequest(r))
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func respondWith(obj interface{}, info information, status int, w http.ResponseWriter) error {
	data, err := jsonapi.MarshalToJSONWithURLs(obj, info)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	w.Write(data)
	return nil
}

func respondWithPagination(obj interface{}, info information, status int, links map[string]string, w http.ResponseWriter) error {
	data, err := jsonapi.MarshalWithURLs(obj, info)
	if err != nil {
		return err
	}

	data["links"] = links
	result, err := json.Marshal(data)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	w.Write(result)
	return nil
}

func unmarshalJSONRequest(r *http.Request) (map[string]interface{}, error) {
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{}
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func handleError(err error, w http.ResponseWriter) {
	log.Println(err)
	if e, ok := err.(HTTPError); ok {
		http.Error(w, marshalError(e), e.status)
		return

	}

	http.Error(w, marshalError(err), http.StatusInternalServerError)
}

// Handler returns the http.Handler instance for the API.
func (api *API) Handler() http.Handler {
	return api.router
}
