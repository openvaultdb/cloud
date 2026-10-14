// Generated from the Northwind contract and source SQLite PRAGMA metadata at 64961074fc09801cf489ba0d3632d14c463950e3; computed view-expression types are inferred from the pinned view SQL.
import type { D1DatabaseOptions } from "@dalgo/d1";

export const NORTHWIND_ID = "https://demodb.dev/northwind/";
export const NORTHWIND_SCHEMA_VERSION = "1";
export const NORTHWIND_SEED_VERSION = "279b34136771aee75d802094b3329515a2b01da65d2a20a4a9e3b58c29b4fd20";

export const NORTHWIND_TABLES = {
  "Categories": {
    "table": "Categories",
    "columns": {
      "CategoryID": {
        "column": "CategoryID"
      },
      "CategoryName": {
        "column": "CategoryName"
      },
      "Description": {
        "column": "Description"
      },
      "Picture": {
        "column": "Picture"
      }
    },
    "primaryKey": [
      "CategoryID"
    ]
  },
  "CustomerCustomerDemo": {
    "table": "CustomerCustomerDemo",
    "columns": {
      "CustomerID": {
        "column": "CustomerID"
      },
      "CustomerTypeID": {
        "column": "CustomerTypeID"
      }
    },
    "primaryKey": [
      "CustomerID",
      "CustomerTypeID"
    ]
  },
  "CustomerDemographics": {
    "table": "CustomerDemographics",
    "columns": {
      "CustomerTypeID": {
        "column": "CustomerTypeID"
      },
      "CustomerDesc": {
        "column": "CustomerDesc"
      }
    },
    "primaryKey": [
      "CustomerTypeID"
    ]
  },
  "Customers": {
    "table": "Customers",
    "columns": {
      "CustomerID": {
        "column": "CustomerID"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "ContactName": {
        "column": "ContactName"
      },
      "ContactTitle": {
        "column": "ContactTitle"
      },
      "Address": {
        "column": "Address"
      },
      "City": {
        "column": "City"
      },
      "Region": {
        "column": "Region"
      },
      "PostalCode": {
        "column": "PostalCode"
      },
      "Country": {
        "column": "Country"
      },
      "Phone": {
        "column": "Phone"
      },
      "Fax": {
        "column": "Fax"
      }
    },
    "primaryKey": [
      "CustomerID"
    ]
  },
  "EmployeeTerritories": {
    "table": "EmployeeTerritories",
    "columns": {
      "EmployeeID": {
        "column": "EmployeeID"
      },
      "TerritoryID": {
        "column": "TerritoryID"
      }
    },
    "primaryKey": [
      "EmployeeID",
      "TerritoryID"
    ]
  },
  "Employees": {
    "table": "Employees",
    "columns": {
      "EmployeeID": {
        "column": "EmployeeID"
      },
      "LastName": {
        "column": "LastName"
      },
      "FirstName": {
        "column": "FirstName"
      },
      "Title": {
        "column": "Title"
      },
      "TitleOfCourtesy": {
        "column": "TitleOfCourtesy"
      },
      "BirthDate": {
        "column": "BirthDate"
      },
      "HireDate": {
        "column": "HireDate"
      },
      "Address": {
        "column": "Address"
      },
      "City": {
        "column": "City"
      },
      "Region": {
        "column": "Region"
      },
      "PostalCode": {
        "column": "PostalCode"
      },
      "Country": {
        "column": "Country"
      },
      "HomePhone": {
        "column": "HomePhone"
      },
      "Extension": {
        "column": "Extension"
      },
      "Photo": {
        "column": "Photo"
      },
      "Notes": {
        "column": "Notes"
      },
      "ReportsTo": {
        "column": "ReportsTo"
      },
      "PhotoPath": {
        "column": "PhotoPath"
      }
    },
    "primaryKey": [
      "EmployeeID"
    ]
  },
  "Order Details": {
    "table": "Order Details",
    "columns": {
      "OrderID": {
        "column": "OrderID"
      },
      "ProductID": {
        "column": "ProductID"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "Quantity": {
        "column": "Quantity"
      },
      "Discount": {
        "column": "Discount"
      }
    },
    "primaryKey": [
      "OrderID",
      "ProductID"
    ]
  },
  "Orders": {
    "table": "Orders",
    "columns": {
      "OrderID": {
        "column": "OrderID"
      },
      "CustomerID": {
        "column": "CustomerID"
      },
      "EmployeeID": {
        "column": "EmployeeID"
      },
      "OrderDate": {
        "column": "OrderDate"
      },
      "RequiredDate": {
        "column": "RequiredDate"
      },
      "ShippedDate": {
        "column": "ShippedDate"
      },
      "ShipVia": {
        "column": "ShipVia"
      },
      "Freight": {
        "column": "Freight"
      },
      "ShipName": {
        "column": "ShipName"
      },
      "ShipAddress": {
        "column": "ShipAddress"
      },
      "ShipCity": {
        "column": "ShipCity"
      },
      "ShipRegion": {
        "column": "ShipRegion"
      },
      "ShipPostalCode": {
        "column": "ShipPostalCode"
      },
      "ShipCountry": {
        "column": "ShipCountry"
      }
    },
    "primaryKey": [
      "OrderID"
    ]
  },
  "Products": {
    "table": "Products",
    "columns": {
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "SupplierID": {
        "column": "SupplierID"
      },
      "CategoryID": {
        "column": "CategoryID"
      },
      "QuantityPerUnit": {
        "column": "QuantityPerUnit"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "UnitsInStock": {
        "column": "UnitsInStock"
      },
      "UnitsOnOrder": {
        "column": "UnitsOnOrder"
      },
      "ReorderLevel": {
        "column": "ReorderLevel"
      },
      "Discontinued": {
        "column": "Discontinued"
      }
    },
    "primaryKey": [
      "ProductID"
    ]
  },
  "Regions": {
    "table": "Regions",
    "columns": {
      "RegionID": {
        "column": "RegionID"
      },
      "RegionDescription": {
        "column": "RegionDescription"
      }
    },
    "primaryKey": [
      "RegionID"
    ]
  },
  "Shippers": {
    "table": "Shippers",
    "columns": {
      "ShipperID": {
        "column": "ShipperID"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "Phone": {
        "column": "Phone"
      }
    },
    "primaryKey": [
      "ShipperID"
    ]
  },
  "Suppliers": {
    "table": "Suppliers",
    "columns": {
      "SupplierID": {
        "column": "SupplierID"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "ContactName": {
        "column": "ContactName"
      },
      "ContactTitle": {
        "column": "ContactTitle"
      },
      "Address": {
        "column": "Address"
      },
      "City": {
        "column": "City"
      },
      "Region": {
        "column": "Region"
      },
      "PostalCode": {
        "column": "PostalCode"
      },
      "Country": {
        "column": "Country"
      },
      "Phone": {
        "column": "Phone"
      },
      "Fax": {
        "column": "Fax"
      },
      "HomePage": {
        "column": "HomePage"
      }
    },
    "primaryKey": [
      "SupplierID"
    ]
  },
  "Territories": {
    "table": "Territories",
    "columns": {
      "TerritoryID": {
        "column": "TerritoryID"
      },
      "TerritoryDescription": {
        "column": "TerritoryDescription"
      },
      "RegionID": {
        "column": "RegionID"
      }
    },
    "primaryKey": [
      "TerritoryID"
    ]
  },
  "Alphabetical list of products": {
    "table": "Alphabetical list of products",
    "columns": {
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "SupplierID": {
        "column": "SupplierID"
      },
      "CategoryID": {
        "column": "CategoryID"
      },
      "QuantityPerUnit": {
        "column": "QuantityPerUnit"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "UnitsInStock": {
        "column": "UnitsInStock"
      },
      "UnitsOnOrder": {
        "column": "UnitsOnOrder"
      },
      "ReorderLevel": {
        "column": "ReorderLevel"
      },
      "Discontinued": {
        "column": "Discontinued"
      },
      "CategoryName": {
        "column": "CategoryName"
      }
    },
    "primaryKey": []
  },
  "Category Sales for 1997": {
    "table": "Category Sales for 1997",
    "columns": {
      "CategoryName": {
        "column": "CategoryName"
      },
      "CategorySales": {
        "column": "CategorySales"
      }
    },
    "primaryKey": []
  },
  "Current Product List": {
    "table": "Current Product List",
    "columns": {
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      }
    },
    "primaryKey": []
  },
  "Customer and Suppliers by City": {
    "table": "Customer and Suppliers by City",
    "columns": {
      "City": {
        "column": "City"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "ContactName": {
        "column": "ContactName"
      },
      "Relationship": {
        "column": "Relationship"
      }
    },
    "primaryKey": []
  },
  "Invoices": {
    "table": "Invoices",
    "columns": {
      "ShipName": {
        "column": "ShipName"
      },
      "ShipAddress": {
        "column": "ShipAddress"
      },
      "ShipCity": {
        "column": "ShipCity"
      },
      "ShipRegion": {
        "column": "ShipRegion"
      },
      "ShipPostalCode": {
        "column": "ShipPostalCode"
      },
      "ShipCountry": {
        "column": "ShipCountry"
      },
      "CustomerID": {
        "column": "CustomerID"
      },
      "CustomerName": {
        "column": "CustomerName"
      },
      "Address": {
        "column": "Address"
      },
      "City": {
        "column": "City"
      },
      "Region": {
        "column": "Region"
      },
      "PostalCode": {
        "column": "PostalCode"
      },
      "Country": {
        "column": "Country"
      },
      "Salesperson": {
        "column": "Salesperson"
      },
      "OrderID": {
        "column": "OrderID"
      },
      "OrderDate": {
        "column": "OrderDate"
      },
      "RequiredDate": {
        "column": "RequiredDate"
      },
      "ShippedDate": {
        "column": "ShippedDate"
      },
      "ShipperName": {
        "column": "ShipperName"
      },
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "Quantity": {
        "column": "Quantity"
      },
      "Discount": {
        "column": "Discount"
      },
      "ExtendedPrice": {
        "column": "ExtendedPrice"
      },
      "Freight": {
        "column": "Freight"
      }
    },
    "primaryKey": []
  },
  "Order Details Extended": {
    "table": "Order Details Extended",
    "columns": {
      "OrderID": {
        "column": "OrderID"
      },
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "Quantity": {
        "column": "Quantity"
      },
      "Discount": {
        "column": "Discount"
      },
      "ExtendedPrice": {
        "column": "ExtendedPrice"
      }
    },
    "primaryKey": []
  },
  "Order Subtotals": {
    "table": "Order Subtotals",
    "columns": {
      "OrderID": {
        "column": "OrderID"
      },
      "Subtotal": {
        "column": "Subtotal"
      }
    },
    "primaryKey": []
  },
  "Orders Qry": {
    "table": "Orders Qry",
    "columns": {
      "OrderID": {
        "column": "OrderID"
      },
      "CustomerID": {
        "column": "CustomerID"
      },
      "EmployeeID": {
        "column": "EmployeeID"
      },
      "OrderDate": {
        "column": "OrderDate"
      },
      "RequiredDate": {
        "column": "RequiredDate"
      },
      "ShippedDate": {
        "column": "ShippedDate"
      },
      "ShipVia": {
        "column": "ShipVia"
      },
      "Freight": {
        "column": "Freight"
      },
      "ShipName": {
        "column": "ShipName"
      },
      "ShipAddress": {
        "column": "ShipAddress"
      },
      "ShipCity": {
        "column": "ShipCity"
      },
      "ShipRegion": {
        "column": "ShipRegion"
      },
      "ShipPostalCode": {
        "column": "ShipPostalCode"
      },
      "ShipCountry": {
        "column": "ShipCountry"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "Address": {
        "column": "Address"
      },
      "City": {
        "column": "City"
      },
      "Region": {
        "column": "Region"
      },
      "PostalCode": {
        "column": "PostalCode"
      },
      "Country": {
        "column": "Country"
      }
    },
    "primaryKey": []
  },
  "Product Sales for 1997": {
    "table": "Product Sales for 1997",
    "columns": {
      "CategoryName": {
        "column": "CategoryName"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "ProductSales": {
        "column": "ProductSales"
      }
    },
    "primaryKey": []
  },
  "ProductDetails_V": {
    "table": "ProductDetails_V",
    "columns": {
      "ProductID": {
        "column": "ProductID"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "SupplierID": {
        "column": "SupplierID"
      },
      "CategoryID": {
        "column": "CategoryID"
      },
      "QuantityPerUnit": {
        "column": "QuantityPerUnit"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      },
      "UnitsInStock": {
        "column": "UnitsInStock"
      },
      "UnitsOnOrder": {
        "column": "UnitsOnOrder"
      },
      "ReorderLevel": {
        "column": "ReorderLevel"
      },
      "Discontinued": {
        "column": "Discontinued"
      },
      "CategoryName": {
        "column": "CategoryName"
      },
      "CategoryDescription": {
        "column": "CategoryDescription"
      },
      "SupplierName": {
        "column": "SupplierName"
      },
      "SupplierRegion": {
        "column": "SupplierRegion"
      }
    },
    "primaryKey": []
  },
  "Products Above Average Price": {
    "table": "Products Above Average Price",
    "columns": {
      "ProductName": {
        "column": "ProductName"
      },
      "UnitPrice": {
        "column": "UnitPrice"
      }
    },
    "primaryKey": []
  },
  "Products by Category": {
    "table": "Products by Category",
    "columns": {
      "CategoryName": {
        "column": "CategoryName"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "QuantityPerUnit": {
        "column": "QuantityPerUnit"
      },
      "UnitsInStock": {
        "column": "UnitsInStock"
      },
      "Discontinued": {
        "column": "Discontinued"
      }
    },
    "primaryKey": []
  },
  "Quarterly Orders": {
    "table": "Quarterly Orders",
    "columns": {
      "CustomerID": {
        "column": "CustomerID"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "City": {
        "column": "City"
      },
      "Country": {
        "column": "Country"
      }
    },
    "primaryKey": []
  },
  "Sales Totals by Amount": {
    "table": "Sales Totals by Amount",
    "columns": {
      "SaleAmount": {
        "column": "SaleAmount"
      },
      "OrderID": {
        "column": "OrderID"
      },
      "CompanyName": {
        "column": "CompanyName"
      },
      "ShippedDate": {
        "column": "ShippedDate"
      }
    },
    "primaryKey": []
  },
  "Sales by Category": {
    "table": "Sales by Category",
    "columns": {
      "CategoryID": {
        "column": "CategoryID"
      },
      "CategoryName": {
        "column": "CategoryName"
      },
      "ProductName": {
        "column": "ProductName"
      },
      "ProductSales": {
        "column": "ProductSales"
      }
    },
    "primaryKey": []
  },
  "Summary of Sales by Quarter": {
    "table": "Summary of Sales by Quarter",
    "columns": {
      "ShippedDate": {
        "column": "ShippedDate"
      },
      "OrderID": {
        "column": "OrderID"
      },
      "Subtotal": {
        "column": "Subtotal"
      }
    },
    "primaryKey": []
  },
  "Summary of Sales by Year": {
    "table": "Summary of Sales by Year",
    "columns": {
      "ShippedDate": {
        "column": "ShippedDate"
      },
      "OrderID": {
        "column": "OrderID"
      },
      "Subtotal": {
        "column": "Subtotal"
      }
    },
    "primaryKey": []
  }
} as const satisfies D1DatabaseOptions["tables"];

export const NORTHWIND_COLUMN_TYPES = {
  "Alphabetical list of products": {
    "ProductID": "INTEGER",
    "ProductName": "TEXT",
    "SupplierID": "INTEGER",
    "CategoryID": "INTEGER",
    "QuantityPerUnit": "TEXT",
    "UnitPrice": "NUMERIC",
    "UnitsInStock": "INTEGER",
    "UnitsOnOrder": "INTEGER",
    "ReorderLevel": "INTEGER",
    "Discontinued": "TEXT",
    "CategoryName": "TEXT"
  },
  "Categories": {
    "CategoryID": "INTEGER",
    "CategoryName": "TEXT",
    "Description": "TEXT",
    "Picture": "BLOB"
  },
  "Category Sales for 1997": {
    "CategoryName": "TEXT",
    "CategorySales": "NUMERIC"
  },
  "Current Product List": {
    "ProductID": "INTEGER",
    "ProductName": "TEXT"
  },
  "Customer and Suppliers by City": {
    "City": "TEXT",
    "CompanyName": "TEXT",
    "ContactName": "TEXT",
    "Relationship": "TEXT"
  },
  "CustomerCustomerDemo": {
    "CustomerID": "TEXT",
    "CustomerTypeID": "TEXT"
  },
  "CustomerDemographics": {
    "CustomerTypeID": "TEXT",
    "CustomerDesc": "TEXT"
  },
  "Customers": {
    "CustomerID": "TEXT",
    "CompanyName": "TEXT",
    "ContactName": "TEXT",
    "ContactTitle": "TEXT",
    "Address": "TEXT",
    "City": "TEXT",
    "Region": "TEXT",
    "PostalCode": "TEXT",
    "Country": "TEXT",
    "Phone": "TEXT",
    "Fax": "TEXT"
  },
  "EmployeeTerritories": {
    "EmployeeID": "INTEGER",
    "TerritoryID": "TEXT"
  },
  "Employees": {
    "EmployeeID": "INTEGER",
    "LastName": "TEXT",
    "FirstName": "TEXT",
    "Title": "TEXT",
    "TitleOfCourtesy": "TEXT",
    "BirthDate": "DATE",
    "HireDate": "DATE",
    "Address": "TEXT",
    "City": "TEXT",
    "Region": "TEXT",
    "PostalCode": "TEXT",
    "Country": "TEXT",
    "HomePhone": "TEXT",
    "Extension": "TEXT",
    "Photo": "BLOB",
    "Notes": "TEXT",
    "ReportsTo": "INTEGER",
    "PhotoPath": "TEXT"
  },
  "Invoices": {
    "ShipName": "TEXT",
    "ShipAddress": "TEXT",
    "ShipCity": "TEXT",
    "ShipRegion": "TEXT",
    "ShipPostalCode": "TEXT",
    "ShipCountry": "TEXT",
    "CustomerID": "TEXT",
    "CustomerName": "TEXT",
    "Address": "TEXT",
    "City": "TEXT",
    "Region": "TEXT",
    "PostalCode": "TEXT",
    "Country": "TEXT",
    "Salesperson": "TEXT",
    "OrderID": "INTEGER",
    "OrderDate": "DATETIME",
    "RequiredDate": "DATETIME",
    "ShippedDate": "DATETIME",
    "ShipperName": "TEXT",
    "ProductID": "INTEGER",
    "ProductName": "TEXT",
    "UnitPrice": "NUMERIC",
    "Quantity": "INTEGER",
    "Discount": "REAL",
    "ExtendedPrice": "NUMERIC",
    "Freight": "NUMERIC"
  },
  "Order Details": {
    "OrderID": "INTEGER",
    "ProductID": "INTEGER",
    "UnitPrice": "NUMERIC",
    "Quantity": "INTEGER",
    "Discount": "REAL"
  },
  "Order Details Extended": {
    "OrderID": "INTEGER",
    "ProductID": "INTEGER",
    "ProductName": "TEXT",
    "UnitPrice": "NUMERIC",
    "Quantity": "INTEGER",
    "Discount": "REAL",
    "ExtendedPrice": "NUMERIC"
  },
  "Order Subtotals": {
    "OrderID": "INTEGER",
    "Subtotal": "NUMERIC"
  },
  "Orders": {
    "OrderID": "INTEGER",
    "CustomerID": "TEXT",
    "EmployeeID": "INTEGER",
    "OrderDate": "DATETIME",
    "RequiredDate": "DATETIME",
    "ShippedDate": "DATETIME",
    "ShipVia": "INTEGER",
    "Freight": "NUMERIC",
    "ShipName": "TEXT",
    "ShipAddress": "TEXT",
    "ShipCity": "TEXT",
    "ShipRegion": "TEXT",
    "ShipPostalCode": "TEXT",
    "ShipCountry": "TEXT"
  },
  "Orders Qry": {
    "OrderID": "INTEGER",
    "CustomerID": "TEXT",
    "EmployeeID": "INTEGER",
    "OrderDate": "DATETIME",
    "RequiredDate": "DATETIME",
    "ShippedDate": "DATETIME",
    "ShipVia": "INTEGER",
    "Freight": "NUMERIC",
    "ShipName": "TEXT",
    "ShipAddress": "TEXT",
    "ShipCity": "TEXT",
    "ShipRegion": "TEXT",
    "ShipPostalCode": "TEXT",
    "ShipCountry": "TEXT",
    "CompanyName": "TEXT",
    "Address": "TEXT",
    "City": "TEXT",
    "Region": "TEXT",
    "PostalCode": "TEXT",
    "Country": "TEXT"
  },
  "Product Sales for 1997": {
    "CategoryName": "TEXT",
    "ProductName": "TEXT",
    "ProductSales": "NUMERIC"
  },
  "ProductDetails_V": {
    "ProductID": "INTEGER",
    "ProductName": "TEXT",
    "SupplierID": "INTEGER",
    "CategoryID": "INTEGER",
    "QuantityPerUnit": "TEXT",
    "UnitPrice": "NUMERIC",
    "UnitsInStock": "INTEGER",
    "UnitsOnOrder": "INTEGER",
    "ReorderLevel": "INTEGER",
    "Discontinued": "TEXT",
    "CategoryName": "TEXT",
    "CategoryDescription": "TEXT",
    "SupplierName": "TEXT",
    "SupplierRegion": "TEXT"
  },
  "Products": {
    "ProductID": "INTEGER",
    "ProductName": "TEXT",
    "SupplierID": "INTEGER",
    "CategoryID": "INTEGER",
    "QuantityPerUnit": "TEXT",
    "UnitPrice": "NUMERIC",
    "UnitsInStock": "INTEGER",
    "UnitsOnOrder": "INTEGER",
    "ReorderLevel": "INTEGER",
    "Discontinued": "TEXT"
  },
  "Products Above Average Price": {
    "ProductName": "TEXT",
    "UnitPrice": "NUMERIC"
  },
  "Products by Category": {
    "CategoryName": "TEXT",
    "ProductName": "TEXT",
    "QuantityPerUnit": "TEXT",
    "UnitsInStock": "INTEGER",
    "Discontinued": "TEXT"
  },
  "Quarterly Orders": {
    "CustomerID": "TEXT",
    "CompanyName": "TEXT",
    "City": "TEXT",
    "Country": "TEXT"
  },
  "Regions": {
    "RegionID": "INTEGER",
    "RegionDescription": "TEXT"
  },
  "Sales Totals by Amount": {
    "SaleAmount": "NUMERIC",
    "OrderID": "INTEGER",
    "CompanyName": "TEXT",
    "ShippedDate": "DATETIME"
  },
  "Sales by Category": {
    "CategoryID": "INTEGER",
    "CategoryName": "TEXT",
    "ProductName": "TEXT",
    "ProductSales": "NUMERIC"
  },
  "Shippers": {
    "ShipperID": "INTEGER",
    "CompanyName": "TEXT",
    "Phone": "TEXT"
  },
  "Summary of Sales by Quarter": {
    "ShippedDate": "DATETIME",
    "OrderID": "INTEGER",
    "Subtotal": "NUMERIC"
  },
  "Summary of Sales by Year": {
    "ShippedDate": "DATETIME",
    "OrderID": "INTEGER",
    "Subtotal": "NUMERIC"
  },
  "Suppliers": {
    "SupplierID": "INTEGER",
    "CompanyName": "TEXT",
    "ContactName": "TEXT",
    "ContactTitle": "TEXT",
    "Address": "TEXT",
    "City": "TEXT",
    "Region": "TEXT",
    "PostalCode": "TEXT",
    "Country": "TEXT",
    "Phone": "TEXT",
    "Fax": "TEXT",
    "HomePage": "TEXT"
  },
  "Territories": {
    "TerritoryID": "TEXT",
    "TerritoryDescription": "TEXT",
    "RegionID": "INTEGER"
  }
} as const;

export const NORTHWIND_EXPECTED_TABLE_ROWS = {
  "Categories": 8,
  "CustomerCustomerDemo": 0,
  "CustomerDemographics": 0,
  "Customers": 93,
  "EmployeeTerritories": 49,
  "Employees": 9,
  "Order Details": 2155,
  "Orders": 830,
  "Products": 77,
  "Regions": 4,
  "Shippers": 3,
  "Suppliers": 29,
  "Territories": 53
} as const;

export const NORTHWIND_EXPECTED_VIEW_ROWS = {
  "Alphabetical list of products": 69,
  "Category Sales for 1997": 0,
  "Current Product List": 69,
  "Customer and Suppliers by City": 122,
  "Invoices": 2155,
  "Order Details Extended": 2155,
  "Order Subtotals": 830,
  "Orders Qry": 830,
  "Product Sales for 1997": 0,
  "ProductDetails_V": 77,
  "Products Above Average Price": 25,
  "Products by Category": 69,
  "Quarterly Orders": 0,
  "Sales Totals by Amount": 0,
  "Sales by Category": 0,
  "Summary of Sales by Quarter": 809,
  "Summary of Sales by Year": 809
} as const;

export const NORTHWIND_D1_IMPORT_SHA256 = "fea7d05421f0297adc18c41196f22b498c669abf78c2392b50343091df8810d7";

// Column types of the pinned northwind/postgres.sql import, read from information_schema.columns
// (test-node/northwind-postgres.test.ts fails if this drifts). Collations and bound values follow these, not the SQLite types.
export const NORTHWIND_PG_TYPES = {
  "Alphabetical list of products": {
    "ProductID": "bigint",
    "ProductName": "text",
    "SupplierID": "bigint",
    "CategoryID": "bigint",
    "QuantityPerUnit": "text",
    "UnitPrice": "numeric",
    "UnitsInStock": "bigint",
    "UnitsOnOrder": "bigint",
    "ReorderLevel": "bigint",
    "Discontinued": "text",
    "CategoryName": "text"
  },
  "Categories": {
    "CategoryID": "bigint",
    "CategoryName": "text",
    "Description": "text",
    "Picture": "bytea"
  },
  "Category Sales for 1997": {
    "CategoryName": "text",
    "CategorySales": "double precision"
  },
  "Current Product List": {
    "ProductID": "bigint",
    "ProductName": "text"
  },
  "Customer and Suppliers by City": {
    "City": "text",
    "CompanyName": "text",
    "ContactName": "text",
    "Relationship": "text"
  },
  "CustomerCustomerDemo": {
    "CustomerID": "text",
    "CustomerTypeID": "text"
  },
  "CustomerDemographics": {
    "CustomerTypeID": "text",
    "CustomerDesc": "text"
  },
  "Customers": {
    "CustomerID": "text",
    "CompanyName": "text",
    "ContactName": "text",
    "ContactTitle": "text",
    "Address": "text",
    "City": "text",
    "Region": "text",
    "PostalCode": "text",
    "Country": "text",
    "Phone": "text",
    "Fax": "text"
  },
  "EmployeeTerritories": {
    "EmployeeID": "bigint",
    "TerritoryID": "text"
  },
  "Employees": {
    "EmployeeID": "bigint",
    "LastName": "text",
    "FirstName": "text",
    "Title": "text",
    "TitleOfCourtesy": "text",
    "BirthDate": "text",
    "HireDate": "text",
    "Address": "text",
    "City": "text",
    "Region": "text",
    "PostalCode": "text",
    "Country": "text",
    "HomePhone": "text",
    "Extension": "text",
    "Photo": "bytea",
    "Notes": "text",
    "ReportsTo": "bigint",
    "PhotoPath": "text"
  },
  "Invoices": {
    "ShipName": "text",
    "ShipAddress": "text",
    "ShipCity": "text",
    "ShipRegion": "text",
    "ShipPostalCode": "text",
    "ShipCountry": "text",
    "CustomerID": "text",
    "CustomerName": "text",
    "Address": "text",
    "City": "text",
    "Region": "text",
    "PostalCode": "text",
    "Country": "text",
    "Salesperson": "integer",
    "OrderID": "bigint",
    "OrderDate": "text",
    "RequiredDate": "text",
    "ShippedDate": "text",
    "ShipperName": "text",
    "ProductID": "bigint",
    "ProductName": "text",
    "UnitPrice": "double precision",
    "Quantity": "bigint",
    "Discount": "double precision",
    "ExtendedPrice": "double precision",
    "Freight": "numeric"
  },
  "Order Details": {
    "OrderID": "bigint",
    "ProductID": "bigint",
    "UnitPrice": "numeric",
    "Quantity": "bigint",
    "Discount": "double precision"
  },
  "Order Details Extended": {
    "OrderID": "bigint",
    "ProductID": "bigint",
    "ProductName": "text",
    "UnitPrice": "double precision",
    "Quantity": "bigint",
    "Discount": "double precision",
    "ExtendedPrice": "double precision"
  },
  "Order Subtotals": {
    "OrderID": "bigint",
    "Subtotal": "double precision"
  },
  "Orders": {
    "OrderID": "bigint",
    "CustomerID": "text",
    "EmployeeID": "bigint",
    "OrderDate": "text",
    "RequiredDate": "text",
    "ShippedDate": "text",
    "ShipVia": "bigint",
    "Freight": "numeric",
    "ShipName": "text",
    "ShipAddress": "text",
    "ShipCity": "text",
    "ShipRegion": "text",
    "ShipPostalCode": "text",
    "ShipCountry": "text"
  },
  "Orders Qry": {
    "OrderID": "bigint",
    "CustomerID": "text",
    "EmployeeID": "bigint",
    "OrderDate": "text",
    "RequiredDate": "text",
    "ShippedDate": "text",
    "ShipVia": "bigint",
    "Freight": "numeric",
    "ShipName": "text",
    "ShipAddress": "text",
    "ShipCity": "text",
    "ShipRegion": "text",
    "ShipPostalCode": "text",
    "ShipCountry": "text",
    "CompanyName": "text",
    "Address": "text",
    "City": "text",
    "Region": "text",
    "PostalCode": "text",
    "Country": "text"
  },
  "Product Sales for 1997": {
    "CategoryName": "text",
    "ProductName": "text",
    "ProductSales": "double precision"
  },
  "ProductDetails_V": {
    "ProductID": "bigint",
    "ProductName": "text",
    "SupplierID": "bigint",
    "CategoryID": "bigint",
    "QuantityPerUnit": "text",
    "UnitPrice": "numeric",
    "UnitsInStock": "bigint",
    "UnitsOnOrder": "bigint",
    "ReorderLevel": "bigint",
    "Discontinued": "text",
    "CategoryName": "text",
    "CategoryDescription": "text",
    "SupplierName": "text",
    "SupplierRegion": "text"
  },
  "Products": {
    "ProductID": "bigint",
    "ProductName": "text",
    "SupplierID": "bigint",
    "CategoryID": "bigint",
    "QuantityPerUnit": "text",
    "UnitPrice": "numeric",
    "UnitsInStock": "bigint",
    "UnitsOnOrder": "bigint",
    "ReorderLevel": "bigint",
    "Discontinued": "text"
  },
  "Products Above Average Price": {
    "ProductName": "text",
    "UnitPrice": "numeric"
  },
  "Products by Category": {
    "CategoryName": "text",
    "ProductName": "text",
    "QuantityPerUnit": "text",
    "UnitsInStock": "bigint",
    "Discontinued": "text"
  },
  "Quarterly Orders": {
    "CustomerID": "text",
    "CompanyName": "text",
    "City": "text",
    "Country": "text"
  },
  "Regions": {
    "RegionID": "bigint",
    "RegionDescription": "text"
  },
  "Sales Totals by Amount": {
    "SaleAmount": "double precision",
    "OrderID": "bigint",
    "CompanyName": "text",
    "ShippedDate": "text"
  },
  "Sales by Category": {
    "CategoryID": "bigint",
    "CategoryName": "text",
    "ProductName": "text",
    "ProductSales": "double precision"
  },
  "Shippers": {
    "ShipperID": "bigint",
    "CompanyName": "text",
    "Phone": "text"
  },
  "Summary of Sales by Quarter": {
    "ShippedDate": "text",
    "OrderID": "bigint",
    "Subtotal": "double precision"
  },
  "Summary of Sales by Year": {
    "ShippedDate": "text",
    "OrderID": "bigint",
    "Subtotal": "double precision"
  },
  "Suppliers": {
    "SupplierID": "bigint",
    "CompanyName": "text",
    "ContactName": "text",
    "ContactTitle": "text",
    "Address": "text",
    "City": "text",
    "Region": "text",
    "PostalCode": "text",
    "Country": "text",
    "Phone": "text",
    "Fax": "text",
    "HomePage": "text"
  },
  "Territories": {
    "TerritoryID": "text",
    "TerritoryDescription": "text",
    "RegionID": "bigint"
  }
} as const;
