package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/appbaseio-confidential/arc/arc/middleware"
	"github.com/appbaseio-confidential/arc/arc/middleware/order"
	"github.com/appbaseio-confidential/arc/middleware/classify"
	"github.com/appbaseio-confidential/arc/middleware/validate"
	"github.com/appbaseio-confidential/arc/model/category"
	"github.com/appbaseio-confidential/arc/model/index"
	"github.com/appbaseio-confidential/arc/plugins/auth"
	"github.com/appbaseio-confidential/arc/plugins/logs"
	"github.com/appbaseio-confidential/arc/util"
	"github.com/appbaseio-confidential/arc/util/iplookup"
	"github.com/google/uuid"
)

// Custom headers
const (
	XSearchQuery         = "X-Search-Query"
	XSearchID            = "X-Search-Id"
	XSearchFilters       = "X-Search-Filters"
	XSearchClick         = "X-Search-Click"
	XSearchClickPosition = "X-Search-Click-Position"
	XSearchConversion    = "X-Search-Conversion"
	XSearchCustomEvent   = "X-Search-Custom-Event"
)

type chain struct {
	order.Fifo
}

func (c *chain) Wrap(h http.HandlerFunc) http.HandlerFunc {
	return c.Adapt(h, list()...)
}

func list() []middleware.Middleware {
	return []middleware.Middleware{
		classifyCategory,
		classify.Op(),
		classify.Indices(),
		logs.Recorder(),
		auth.BasicAuth(),
		validate.Indices(),
		validate.Operation(),
		validate.Category(),
	}
}

func classifyCategory(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		requestCategory := category.Analytics

		ctx := category.NewContext(req.Context(), &requestCategory)
		req = req.WithContext(ctx)

		h(w, req)
	}
}

type searchResponse struct {
	Took float64 `json:"took"`
	Hits struct {
		Total int `json:"total"`
		Hits  []struct {
			Source map[string]interface{} `json:"source"`
			Type   string                 `json:"type"`
			ID     string                 `json:"id"`
		} `json:"hits"`
	} `json:"hits"`
}

type mSearchResponse struct {
	Responses []searchResponse `json:"responses"`
}

// Recorder parses and records the search requests made to elasticsearch along with some other
// user information in order to calculate and serve useful analytics.
func Recorder() middleware.Middleware {
	return Instance().recorder
}

func (a *Analytics) recorder(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		reqACL, err := category.FromContext(ctx)
		if err != nil {
			log.Printf("%s: %v", logTag, err)
			util.WriteBackError(w, "an error occurred while recording search request", http.StatusInternalServerError)
			return
		}
		//decode headers and set it back if it relates to analytics
		for header, _ := range r.Header {
			if strings.HasPrefix(header, "X-Search-") {
				parsedValue, err := url.QueryUnescape(r.Header.Get(header))
				if err != nil {
					log.Println("error while decoding header: ", err)
					h(w, r)
					return
				}
				r.Header.Set(header, parsedValue)
			}
		}
		searchQuery := r.Header.Get(XSearchQuery)
		searchID := r.Header.Get(XSearchID)
		if *reqACL != category.Search || (searchQuery == "" && searchID == "") {
			h(w, r)
			return
		}

		docID := searchID
		if docID == "" {
			docID = uuid.New().String()
		}

		// serve using response recorder
		respRecorder := httptest.NewRecorder()
		h(respRecorder, r)

		// copy the response to writer
		for k, v := range respRecorder.Header() {
			w.Header()[k] = v
		}
		w.Header().Set(XSearchID, docID)
		w.WriteHeader(respRecorder.Code)
		w.Write(respRecorder.Body.Bytes())

		// record the search response
		go a.recordResponse(docID, searchID, respRecorder, r)
	}
}

// TODO: For urls ending with _search or _msearch? Stricter checks should make it hard to misuse.
func (a *Analytics) recordResponse(docID, searchID string, w *httptest.ResponseRecorder, r *http.Request) {
	// read the response from elasticsearch
	respBody, err := ioutil.ReadAll(w.Result().Body)
	if err != nil {
		log.Printf("%s: can't read response body, unable to record es response: %v\n", logTag, err)
		return
	}

	// replace es response fields
	respBody = bytes.Replace(respBody, []byte("_source"), []byte("source"), -1)
	respBody = bytes.Replace(respBody, []byte("_type"), []byte("type"), -1)
	respBody = bytes.Replace(respBody, []byte("_id"), []byte("id"), -1)

	var esResponse searchResponse
	if strings.Contains(r.RequestURI, "_msearch") {
		var m mSearchResponse
		err := json.Unmarshal(respBody, &m)
		if err != nil {
			log.Printf(`%s: can't unmarshal "_msearch" reponse, unable to record es response %s: %v`,
				logTag, string(respBody), err)
			return
		}
		// TODO: why record only the first _msearch response?
		if len(m.Responses) > 0 {
			esResponse = m.Responses[0]
		}
	} else {
		err := json.Unmarshal(respBody, &esResponse)
		if err != nil {
			log.Printf(`%s: can't unmarshal "_search" reponse, unable to record es response %s: %v`,
				logTag, string(respBody), err)
			return
		}
	}

	// record up to top 10 hits
	var hits []map[string]string
	for i := 0; i < 10 && i < len(esResponse.Hits.Hits); i++ {
		source := esResponse.Hits.Hits[i].Source
		raw, err := json.Marshal(source)
		if err != nil {
			log.Printf("%s: unable to marshal es response source %s: %v\n", logTag, source, err)
			continue
		}

		hit := make(map[string]string)
		hit["id"] = esResponse.Hits.Hits[i].ID
		hit["type"] = esResponse.Hits.Hits[i].Type
		hit["source"] = string(raw)
		hits = append(hits, hit)
	}

	record := make(map[string]interface{})
	record["took"] = esResponse.Took
	if searchID == "" {
		ctxIndices, err := index.FromContext(r.Context())
		if err != nil {
			log.Printf("%s: cannot fetch indices from request context, %v\n", logTag, err)
			return
		}

		record["indices"] = ctxIndices
		record["search_query"] = r.Header.Get(XSearchQuery)
		record["hits_in_response"] = hits
		record["total_hits"] = esResponse.Hits.Total
		record["timestamp"] = time.Now().Format(time.RFC3339)

		searchFilters := parse(r.Header.Get(XSearchFilters))
		if len(searchFilters) > 0 {
			record["search_filters"] = searchFilters
		}
	}

	ipAddr := iplookup.FromRequest(r)
	record["ip"] = ipAddr
	ipInfo := iplookup.Instance()

	coordinates, err := ipInfo.GetCoordinates(ipAddr)
	if err != nil {
		log.Printf("%s: error fetching location coordinates for ip=%s: %v\n", logTag, ipAddr, err)
	} else {
		record["location"] = coordinates
	}

	country, err := ipInfo.Get(iplookup.Country, ipAddr)
	if err != nil {
		log.Printf("%s: error fetching country for ip=%s: %v\n", logTag, ipAddr, err)
	} else {
		record["country"] = country
	}

	searchClick := r.Header.Get(XSearchClick)
	if searchClick != "" {
		if clicked, err := strconv.ParseBool(searchClick); err == nil {
			record["click"] = clicked
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v\n",
				logTag, searchClick, XSearchClick, err)
		}
	}

	searchClickPosition := r.Header.Get(XSearchClickPosition)
	if searchClickPosition != "" {
		if pos, err := strconv.Atoi(searchClickPosition); err == nil {
			record["click_position"] = pos
		} else {
			log.Printf("%s: invalid int value '%v' passed for header %s: %v\n",
				logTag, searchClickPosition, XSearchClickPosition, err)
		}
	}

	searchConversion := r.Header.Get(XSearchConversion)
	if searchConversion != "" {
		if conversion, err := strconv.ParseBool(searchConversion); err == nil {
			record["conversion"] = conversion
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v\n",
				logTag, searchConversion, XSearchConversion, err)
		}
	}

	customEvents := parse(r.Header.Get(XSearchCustomEvent))
	if len(customEvents) > 0 {
		record["custom_events"] = customEvents
	}

	// TODO: remove
	//logRaw(record)
	a.es.indexRecord(context.Background(), docID, record)
}
