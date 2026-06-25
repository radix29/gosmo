-- ============================================================
-- test_db.sql
-- Creates 3 SQL Server databases with realistic schemas,
-- views, stored procedures, logins, and users.
-- ============================================================

USE [master];
GO

-- ============================================================
-- SECTION 1: CREATE DATABASES
-- ============================================================

-- 1. RetailShop
IF EXISTS (SELECT name FROM sys.databases WHERE name = N'RetailShop')
BEGIN
    ALTER DATABASE [RetailShop] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE [RetailShop];
END
GO
CREATE DATABASE [RetailShop]
ON PRIMARY
(
    NAME     = N'RetailShop_Data',
    FILENAME = N'C:\SQLData\RetailShop_Data.mdf',
    SIZE     = 128MB,
    MAXSIZE  = UNLIMITED,
    FILEGROWTH = 64MB
)
LOG ON
(
    NAME     = N'RetailShop_Log',
    FILENAME = N'C:\SQLData\RetailShop_Log.ldf',
    SIZE     = 64MB,
    MAXSIZE  = 2GB,
    FILEGROWTH = 32MB
);
GO

-- 2. HRManagement
IF EXISTS (SELECT name FROM sys.databases WHERE name = N'HRManagement')
BEGIN
    ALTER DATABASE [HRManagement] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE [HRManagement];
END
GO
CREATE DATABASE [HRManagement]
ON PRIMARY
(
    NAME     = N'HRManagement_Data',
    FILENAME = N'C:\SQLData\HRManagement_Data.mdf',
    SIZE     = 128MB,
    MAXSIZE  = UNLIMITED,
    FILEGROWTH = 64MB
)
LOG ON
(
    NAME     = N'HRManagement_Log',
    FILENAME = N'C:\SQLData\HRManagement_Log.ldf',
    SIZE     = 64MB,
    MAXSIZE  = 2GB,
    FILEGROWTH = 32MB
);
GO

-- 3. HealthClinic
IF EXISTS (SELECT name FROM sys.databases WHERE name = N'HealthClinic')
BEGIN
    ALTER DATABASE [HealthClinic] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE [HealthClinic];
END
GO
CREATE DATABASE [HealthClinic]
ON PRIMARY
(
    NAME     = N'HealthClinic_Data',
    FILENAME = N'C:\SQLData\HealthClinic_Data.mdf',
    SIZE     = 128MB,
    MAXSIZE  = UNLIMITED,
    FILEGROWTH = 64MB
)
LOG ON
(
    NAME     = N'HealthClinic_Log',
    FILENAME = N'C:\SQLData\HealthClinic_Log.ldf',
    SIZE     = 64MB,
    MAXSIZE  = 2GB,
    FILEGROWTH = 32MB
);
GO


-- ============================================================
-- SECTION 2: RetailShop — TABLES, VIEWS, STORED PROCEDURES
-- ============================================================
USE [RetailShop];
GO

-- ---- Tables ----

CREATE TABLE dbo.Categories (
    CategoryID   INT           NOT NULL IDENTITY(1,1),
    CategoryName NVARCHAR(100) NOT NULL,
    Description  NVARCHAR(500)     NULL,
    IsActive     BIT           NOT NULL DEFAULT 1,
    CreatedAt    DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Categories PRIMARY KEY CLUSTERED (CategoryID),
    CONSTRAINT UQ_Categories_Name UNIQUE (CategoryName)
);
GO

CREATE TABLE dbo.Suppliers (
    SupplierID   INT           NOT NULL IDENTITY(1,1),
    CompanyName  NVARCHAR(150) NOT NULL,
    ContactName  NVARCHAR(100)     NULL,
    Phone        NVARCHAR(30)      NULL,
    Email        NVARCHAR(200)     NULL,
    Country      NVARCHAR(80)      NULL,
    CreatedAt    DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Suppliers PRIMARY KEY CLUSTERED (SupplierID)
);
GO
CREATE NONCLUSTERED INDEX IX_Suppliers_Country ON dbo.Suppliers (Country);
GO

CREATE TABLE dbo.Products (
    ProductID    INT             NOT NULL IDENTITY(1,1),
    CategoryID   INT             NOT NULL,
    SupplierID   INT                 NULL,
    ProductName  NVARCHAR(200)   NOT NULL,
    SKU          NVARCHAR(50)    NOT NULL,
    UnitPrice    DECIMAL(10, 2)  NOT NULL DEFAULT 0.00,
    StockQty     INT             NOT NULL DEFAULT 0,
    IsActive     BIT             NOT NULL DEFAULT 1,
    CreatedAt    DATETIME2       NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Products PRIMARY KEY CLUSTERED (ProductID),
    CONSTRAINT UQ_Products_SKU  UNIQUE (SKU),
    CONSTRAINT FK_Products_Categories FOREIGN KEY (CategoryID)
        REFERENCES dbo.Categories (CategoryID),
    CONSTRAINT FK_Products_Suppliers  FOREIGN KEY (SupplierID)
        REFERENCES dbo.Suppliers  (SupplierID)
);
GO
CREATE NONCLUSTERED INDEX IX_Products_CategoryID ON dbo.Products (CategoryID);
CREATE NONCLUSTERED INDEX IX_Products_SupplierID ON dbo.Products (SupplierID);
GO

CREATE TABLE dbo.Customers (
    CustomerID  INT           NOT NULL IDENTITY(1,1),
    FirstName   NVARCHAR(100) NOT NULL,
    LastName    NVARCHAR(100) NOT NULL,
    Email       NVARCHAR(200) NOT NULL,
    Phone       NVARCHAR(30)      NULL,
    City        NVARCHAR(100)     NULL,
    Country     NVARCHAR(80)      NULL,
    CreatedAt   DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Customers PRIMARY KEY CLUSTERED (CustomerID),
    CONSTRAINT UQ_Customers_Email UNIQUE (Email)
);
GO
CREATE NONCLUSTERED INDEX IX_Customers_LastName ON dbo.Customers (LastName, FirstName);
GO

CREATE TABLE dbo.Orders (
    OrderID     INT           NOT NULL IDENTITY(1,1),
    CustomerID  INT           NOT NULL,
    OrderDate   DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    Status      NVARCHAR(50)  NOT NULL DEFAULT 'Pending',
    TotalAmount DECIMAL(12,2) NOT NULL DEFAULT 0.00,
    Notes       NVARCHAR(500)     NULL,
    CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED (OrderID),
    CONSTRAINT FK_Orders_Customers FOREIGN KEY (CustomerID)
        REFERENCES dbo.Customers (CustomerID)
);
GO
CREATE NONCLUSTERED INDEX IX_Orders_CustomerID ON dbo.Orders (CustomerID);
CREATE NONCLUSTERED INDEX IX_Orders_OrderDate   ON dbo.Orders (OrderDate DESC);
GO

CREATE TABLE dbo.OrderLines (
    OrderLineID INT            NOT NULL IDENTITY(1,1),
    OrderID     INT            NOT NULL,
    ProductID   INT            NOT NULL,
    Quantity    INT            NOT NULL DEFAULT 1,
    UnitPrice   DECIMAL(10,2)  NOT NULL,
    Discount    DECIMAL(5,2)   NOT NULL DEFAULT 0.00,
    CONSTRAINT PK_OrderLines PRIMARY KEY CLUSTERED (OrderLineID),
    CONSTRAINT FK_OrderLines_Orders   FOREIGN KEY (OrderID)
        REFERENCES dbo.Orders   (OrderID),
    CONSTRAINT FK_OrderLines_Products FOREIGN KEY (ProductID)
        REFERENCES dbo.Products (ProductID)
);
GO
CREATE NONCLUSTERED INDEX IX_OrderLines_OrderID   ON dbo.OrderLines (OrderID);
CREATE NONCLUSTERED INDEX IX_OrderLines_ProductID ON dbo.OrderLines (ProductID);
GO

CREATE TABLE dbo.Payments (
    PaymentID     INT           NOT NULL IDENTITY(1,1),
    OrderID       INT           NOT NULL,
    PaymentDate   DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    Amount        DECIMAL(12,2) NOT NULL,
    PaymentMethod NVARCHAR(50)  NOT NULL DEFAULT 'CreditCard',
    Reference     NVARCHAR(100)     NULL,
    CONSTRAINT PK_Payments PRIMARY KEY CLUSTERED (PaymentID),
    CONSTRAINT FK_Payments_Orders FOREIGN KEY (OrderID)
        REFERENCES dbo.Orders (OrderID)
);
GO
CREATE NONCLUSTERED INDEX IX_Payments_OrderID ON dbo.Payments (OrderID);
GO

-- ---- Views ----

CREATE OR ALTER VIEW dbo.vw_ProductCatalog
AS
    SELECT
        p.ProductID,
        p.ProductName,
        p.SKU,
        p.UnitPrice,
        p.StockQty,
        c.CategoryName,
        s.CompanyName AS SupplierName
    FROM dbo.Products    p
    JOIN dbo.Categories  c ON c.CategoryID = p.CategoryID
    LEFT JOIN dbo.Suppliers s ON s.SupplierID = p.SupplierID
    WHERE p.IsActive = 1;
GO

CREATE OR ALTER VIEW dbo.vw_OrderSummary
AS
    SELECT
        o.OrderID,
        o.OrderDate,
        o.Status,
        o.TotalAmount,
        cu.CustomerID,
        cu.FirstName + ' ' + cu.LastName AS CustomerName,
        cu.Email,
        COUNT(ol.OrderLineID)            AS LineCount
    FROM dbo.Orders     o
    JOIN dbo.Customers  cu ON cu.CustomerID = o.CustomerID
    LEFT JOIN dbo.OrderLines ol ON ol.OrderID = o.OrderID
    GROUP BY
        o.OrderID, o.OrderDate, o.Status, o.TotalAmount,
        cu.CustomerID, cu.FirstName, cu.LastName, cu.Email;
GO

CREATE OR ALTER VIEW dbo.vw_RevenueByCategory
AS
    SELECT
        c.CategoryName,
        SUM(ol.Quantity * ol.UnitPrice * (1 - ol.Discount / 100)) AS NetRevenue,
        SUM(ol.Quantity)                                           AS UnitsSold,
        COUNT(DISTINCT o.OrderID)                                  AS OrderCount
    FROM dbo.OrderLines  ol
    JOIN dbo.Orders      o  ON o.OrderID    = ol.OrderID
    JOIN dbo.Products    p  ON p.ProductID  = ol.ProductID
    JOIN dbo.Categories  c  ON c.CategoryID = p.CategoryID
    WHERE o.Status <> 'Cancelled'
    GROUP BY c.CategoryName;
GO

-- ---- Stored Procedures ----

CREATE OR ALTER PROCEDURE dbo.usp_CreateOrder
    @CustomerID  INT,
    @Notes       NVARCHAR(500) = NULL,
    @NewOrderID  INT           OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    INSERT INTO dbo.Orders (CustomerID, Status, TotalAmount, Notes)
    VALUES (@CustomerID, 'Pending', 0.00, @Notes);
    SET @NewOrderID = SCOPE_IDENTITY();
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_AddOrderLine
    @OrderID   INT,
    @ProductID INT,
    @Quantity  INT,
    @Discount  DECIMAL(5,2) = 0.00
AS
BEGIN
    SET NOCOUNT ON;
    DECLARE @Price DECIMAL(10,2);
    SELECT @Price = UnitPrice FROM dbo.Products WHERE ProductID = @ProductID;

    IF @Price IS NULL
    BEGIN
        RAISERROR('Product not found.', 16, 1);
        RETURN;
    END

    INSERT INTO dbo.OrderLines (OrderID, ProductID, Quantity, UnitPrice, Discount)
    VALUES (@OrderID, @ProductID, @Quantity, @Price, @Discount);

    -- Recalculate order total
    UPDATE dbo.Orders
    SET TotalAmount = (
        SELECT SUM(Quantity * UnitPrice * (1 - Discount / 100))
        FROM dbo.OrderLines
        WHERE OrderID = @OrderID
    )
    WHERE OrderID = @OrderID;
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_GetCustomerOrders
    @CustomerID INT,
    @FromDate   DATETIME2 = NULL,
    @ToDate     DATETIME2 = NULL
AS
BEGIN
    SET NOCOUNT ON;
    SELECT
        o.OrderID,
        o.OrderDate,
        o.Status,
        o.TotalAmount,
        COUNT(ol.OrderLineID) AS LineCount
    FROM dbo.Orders     o
    LEFT JOIN dbo.OrderLines ol ON ol.OrderID = o.OrderID
    WHERE o.CustomerID = @CustomerID
      AND (@FromDate IS NULL OR o.OrderDate >= @FromDate)
      AND (@ToDate   IS NULL OR o.OrderDate <= @ToDate)
    GROUP BY o.OrderID, o.OrderDate, o.Status, o.TotalAmount
    ORDER BY o.OrderDate DESC;
END;
GO


-- ============================================================
-- SECTION 3: HRManagement — TABLES, VIEWS, STORED PROCEDURES
-- ============================================================
USE [HRManagement];
GO

-- ---- Tables ----

CREATE TABLE dbo.Departments (
    DepartmentID   INT           NOT NULL IDENTITY(1,1),
    DepartmentName NVARCHAR(100) NOT NULL,
    Location       NVARCHAR(100)     NULL,
    Budget         DECIMAL(15,2)     NULL,
    CONSTRAINT PK_Departments PRIMARY KEY CLUSTERED (DepartmentID),
    CONSTRAINT UQ_Departments_Name UNIQUE (DepartmentName)
);
GO

CREATE TABLE dbo.JobTitles (
    JobTitleID   INT           NOT NULL IDENTITY(1,1),
    Title        NVARCHAR(100) NOT NULL,
    MinSalary    DECIMAL(12,2)     NULL,
    MaxSalary    DECIMAL(12,2)     NULL,
    CONSTRAINT PK_JobTitles PRIMARY KEY CLUSTERED (JobTitleID)
);
GO

CREATE TABLE dbo.Employees (
    EmployeeID    INT           NOT NULL IDENTITY(1,1),
    DepartmentID  INT           NOT NULL,
    JobTitleID    INT           NOT NULL,
    ManagerID     INT               NULL,
    FirstName     NVARCHAR(100) NOT NULL,
    LastName      NVARCHAR(100) NOT NULL,
    Email         NVARCHAR(200) NOT NULL,
    HireDate      DATE          NOT NULL,
    Salary        DECIMAL(12,2) NOT NULL DEFAULT 0.00,
    IsActive      BIT           NOT NULL DEFAULT 1,
    CONSTRAINT PK_Employees    PRIMARY KEY CLUSTERED (EmployeeID),
    CONSTRAINT UQ_Employees_Email UNIQUE (Email),
    CONSTRAINT FK_Employees_Departments FOREIGN KEY (DepartmentID)
        REFERENCES dbo.Departments (DepartmentID),
    CONSTRAINT FK_Employees_JobTitles   FOREIGN KEY (JobTitleID)
        REFERENCES dbo.JobTitles   (JobTitleID),
    CONSTRAINT FK_Employees_Manager     FOREIGN KEY (ManagerID)
        REFERENCES dbo.Employees   (EmployeeID)
);
GO
CREATE NONCLUSTERED INDEX IX_Employees_DepartmentID ON dbo.Employees (DepartmentID);
CREATE NONCLUSTERED INDEX IX_Employees_ManagerID    ON dbo.Employees (ManagerID);
CREATE NONCLUSTERED INDEX IX_Employees_LastName     ON dbo.Employees (LastName, FirstName);
GO

CREATE TABLE dbo.LeaveTypes (
    LeaveTypeID   INT           NOT NULL IDENTITY(1,1),
    TypeName      NVARCHAR(80)  NOT NULL,
    MaxDaysPerYear INT          NOT NULL DEFAULT 0,
    CONSTRAINT PK_LeaveTypes PRIMARY KEY CLUSTERED (LeaveTypeID)
);
GO

CREATE TABLE dbo.LeaveRequests (
    LeaveRequestID INT       NOT NULL IDENTITY(1,1),
    EmployeeID     INT       NOT NULL,
    LeaveTypeID    INT       NOT NULL,
    StartDate      DATE      NOT NULL,
    EndDate        DATE      NOT NULL,
    Status         NVARCHAR(30) NOT NULL DEFAULT 'Pending',
    RequestedAt    DATETIME2 NOT NULL DEFAULT SYSDATETIME(),
    ApprovedBy     INT           NULL,
    Notes          NVARCHAR(500) NULL,
    CONSTRAINT PK_LeaveRequests PRIMARY KEY CLUSTERED (LeaveRequestID),
    CONSTRAINT FK_LeaveRequests_Employees   FOREIGN KEY (EmployeeID)
        REFERENCES dbo.Employees  (EmployeeID),
    CONSTRAINT FK_LeaveRequests_LeaveTypes  FOREIGN KEY (LeaveTypeID)
        REFERENCES dbo.LeaveTypes (LeaveTypeID),
    CONSTRAINT FK_LeaveRequests_Approver    FOREIGN KEY (ApprovedBy)
        REFERENCES dbo.Employees  (EmployeeID)
);
GO
CREATE NONCLUSTERED INDEX IX_LeaveRequests_EmployeeID ON dbo.LeaveRequests (EmployeeID);
CREATE NONCLUSTERED INDEX IX_LeaveRequests_Status     ON dbo.LeaveRequests (Status);
GO

CREATE TABLE dbo.PerformanceReviews (
    ReviewID     INT           NOT NULL IDENTITY(1,1),
    EmployeeID   INT           NOT NULL,
    ReviewerID   INT           NOT NULL,
    ReviewDate   DATE          NOT NULL,
    Score        TINYINT       NOT NULL,           -- 1–5
    Comments     NVARCHAR(MAX)     NULL,
    CONSTRAINT PK_PerformanceReviews PRIMARY KEY CLUSTERED (ReviewID),
    CONSTRAINT FK_Reviews_Employee FOREIGN KEY (EmployeeID)
        REFERENCES dbo.Employees (EmployeeID),
    CONSTRAINT FK_Reviews_Reviewer FOREIGN KEY (ReviewerID)
        REFERENCES dbo.Employees (EmployeeID),
    CONSTRAINT CK_Reviews_Score CHECK (Score BETWEEN 1 AND 5)
);
GO
CREATE NONCLUSTERED INDEX IX_Reviews_EmployeeID ON dbo.PerformanceReviews (EmployeeID);
GO

CREATE TABLE dbo.Payroll (
    PayrollID    INT           NOT NULL IDENTITY(1,1),
    EmployeeID   INT           NOT NULL,
    PayPeriod    DATE          NOT NULL,    -- First day of the pay period
    GrossPay     DECIMAL(12,2) NOT NULL,
    Deductions   DECIMAL(12,2) NOT NULL DEFAULT 0.00,
    NetPay       AS (GrossPay - Deductions) PERSISTED,
    ProcessedAt  DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Payroll PRIMARY KEY CLUSTERED (PayrollID),
    CONSTRAINT FK_Payroll_Employees FOREIGN KEY (EmployeeID)
        REFERENCES dbo.Employees (EmployeeID)
);
GO
CREATE NONCLUSTERED INDEX IX_Payroll_EmployeeID ON dbo.Payroll (EmployeeID);
CREATE NONCLUSTERED INDEX IX_Payroll_PayPeriod  ON dbo.Payroll (PayPeriod DESC);
GO

-- ---- Views ----

CREATE OR ALTER VIEW dbo.vw_EmployeeDirectory
AS
    SELECT
        e.EmployeeID,
        e.FirstName + ' ' + e.LastName     AS FullName,
        e.Email,
        j.Title                            AS JobTitle,
        d.DepartmentName,
        m.FirstName + ' ' + m.LastName     AS ManagerName,
        e.HireDate,
        e.IsActive
    FROM dbo.Employees   e
    JOIN dbo.Departments d ON d.DepartmentID = e.DepartmentID
    JOIN dbo.JobTitles   j ON j.JobTitleID   = e.JobTitleID
    LEFT JOIN dbo.Employees m ON m.EmployeeID = e.ManagerID;
GO

CREATE OR ALTER VIEW dbo.vw_DepartmentHeadcount
AS
    SELECT
        d.DepartmentName,
        COUNT(e.EmployeeID)     AS Headcount,
        AVG(e.Salary)           AS AvgSalary,
        SUM(e.Salary)           AS TotalSalary,
        d.Budget
    FROM dbo.Departments d
    LEFT JOIN dbo.Employees e ON e.DepartmentID = d.DepartmentID AND e.IsActive = 1
    GROUP BY d.DepartmentName, d.Budget;
GO

CREATE OR ALTER VIEW dbo.vw_PendingLeaveRequests
AS
    SELECT
        lr.LeaveRequestID,
        e.FirstName + ' ' + e.LastName AS EmployeeName,
        d.DepartmentName,
        lt.TypeName                    AS LeaveType,
        lr.StartDate,
        lr.EndDate,
        DATEDIFF(DAY, lr.StartDate, lr.EndDate) + 1 AS DaysRequested,
        lr.RequestedAt
    FROM dbo.LeaveRequests lr
    JOIN dbo.Employees   e  ON e.EmployeeID  = lr.EmployeeID
    JOIN dbo.Departments d  ON d.DepartmentID = e.DepartmentID
    JOIN dbo.LeaveTypes  lt ON lt.LeaveTypeID = lr.LeaveTypeID
    WHERE lr.Status = 'Pending';
GO

-- ---- Stored Procedures ----

CREATE OR ALTER PROCEDURE dbo.usp_HireEmployee
    @DepartmentID INT,
    @JobTitleID   INT,
    @ManagerID    INT           = NULL,
    @FirstName    NVARCHAR(100),
    @LastName     NVARCHAR(100),
    @Email        NVARCHAR(200),
    @HireDate     DATE,
    @Salary       DECIMAL(12,2),
    @NewEmployeeID INT          OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    INSERT INTO dbo.Employees
        (DepartmentID, JobTitleID, ManagerID, FirstName, LastName, Email, HireDate, Salary, IsActive)
    VALUES
        (@DepartmentID, @JobTitleID, @ManagerID, @FirstName, @LastName, @Email, @HireDate, @Salary, 1);
    SET @NewEmployeeID = SCOPE_IDENTITY();
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_ApproveLeaveRequest
    @LeaveRequestID INT,
    @ApprovedBy     INT
AS
BEGIN
    SET NOCOUNT ON;
    UPDATE dbo.LeaveRequests
    SET    Status     = 'Approved',
           ApprovedBy = @ApprovedBy
    WHERE  LeaveRequestID = @LeaveRequestID
      AND  Status = 'Pending';
    IF @@ROWCOUNT = 0
        RAISERROR('Leave request not found or already processed.', 16, 1);
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_GetEmployeePayHistory
    @EmployeeID INT,
    @Year       INT = NULL
AS
BEGIN
    SET NOCOUNT ON;
    SELECT
        p.PayPeriod,
        p.GrossPay,
        p.Deductions,
        p.NetPay,
        p.ProcessedAt
    FROM dbo.Payroll p
    WHERE p.EmployeeID = @EmployeeID
      AND (@Year IS NULL OR YEAR(p.PayPeriod) = @Year)
    ORDER BY p.PayPeriod DESC;
END;
GO


-- ============================================================
-- SECTION 4: HealthClinic — TABLES, VIEWS, STORED PROCEDURES
-- ============================================================
USE [HealthClinic];
GO

-- ---- Tables ----

CREATE TABLE dbo.Specializations (
    SpecializationID   INT           NOT NULL IDENTITY(1,1),
    SpecializationName NVARCHAR(150) NOT NULL,
    CONSTRAINT PK_Specializations PRIMARY KEY CLUSTERED (SpecializationID)
);
GO

CREATE TABLE dbo.Doctors (
    DoctorID         INT           NOT NULL IDENTITY(1,1),
    SpecializationID INT           NOT NULL,
    FirstName        NVARCHAR(100) NOT NULL,
    LastName         NVARCHAR(100) NOT NULL,
    Email            NVARCHAR(200) NOT NULL,
    Phone            NVARCHAR(30)      NULL,
    LicenseNumber    NVARCHAR(50)  NOT NULL,
    IsAvailable      BIT           NOT NULL DEFAULT 1,
    CONSTRAINT PK_Doctors PRIMARY KEY CLUSTERED (DoctorID),
    CONSTRAINT UQ_Doctors_Email         UNIQUE (Email),
    CONSTRAINT UQ_Doctors_LicenseNumber UNIQUE (LicenseNumber),
    CONSTRAINT FK_Doctors_Specializations FOREIGN KEY (SpecializationID)
        REFERENCES dbo.Specializations (SpecializationID)
);
GO
CREATE NONCLUSTERED INDEX IX_Doctors_SpecializationID ON dbo.Doctors (SpecializationID);
GO

CREATE TABLE dbo.Patients (
    PatientID   INT           NOT NULL IDENTITY(1,1),
    FirstName   NVARCHAR(100) NOT NULL,
    LastName    NVARCHAR(100) NOT NULL,
    DateOfBirth DATE          NOT NULL,
    Gender      CHAR(1)           NULL,
    Email       NVARCHAR(200)     NULL,
    Phone       NVARCHAR(30)      NULL,
    Address     NVARCHAR(300)     NULL,
    RegisteredAt DATETIME2    NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Patients PRIMARY KEY CLUSTERED (PatientID)
);
GO
CREATE NONCLUSTERED INDEX IX_Patients_LastName ON dbo.Patients (LastName, FirstName);
GO

CREATE TABLE dbo.Appointments (
    AppointmentID   INT           NOT NULL IDENTITY(1,1),
    PatientID       INT           NOT NULL,
    DoctorID        INT           NOT NULL,
    ScheduledAt     DATETIME2     NOT NULL,
    DurationMinutes INT           NOT NULL DEFAULT 30,
    Status          NVARCHAR(30)  NOT NULL DEFAULT 'Scheduled',
    Reason          NVARCHAR(500)     NULL,
    CONSTRAINT PK_Appointments PRIMARY KEY CLUSTERED (AppointmentID),
    CONSTRAINT FK_Appointments_Patients FOREIGN KEY (PatientID)
        REFERENCES dbo.Patients (PatientID),
    CONSTRAINT FK_Appointments_Doctors  FOREIGN KEY (DoctorID)
        REFERENCES dbo.Doctors  (DoctorID)
);
GO
CREATE NONCLUSTERED INDEX IX_Appointments_PatientID   ON dbo.Appointments (PatientID);
CREATE NONCLUSTERED INDEX IX_Appointments_DoctorID    ON dbo.Appointments (DoctorID);
CREATE NONCLUSTERED INDEX IX_Appointments_ScheduledAt ON dbo.Appointments (ScheduledAt);
GO

CREATE TABLE dbo.MedicalRecords (
    RecordID      INT           NOT NULL IDENTITY(1,1),
    AppointmentID INT           NOT NULL,
    Diagnosis     NVARCHAR(500)     NULL,
    Treatment     NVARCHAR(MAX)     NULL,
    Notes         NVARCHAR(MAX)     NULL,
    CreatedAt     DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    CONSTRAINT PK_MedicalRecords PRIMARY KEY CLUSTERED (RecordID),
    CONSTRAINT FK_MedicalRecords_Appointments FOREIGN KEY (AppointmentID)
        REFERENCES dbo.Appointments (AppointmentID)
);
GO

CREATE TABLE dbo.Medications (
    MedicationID   INT           NOT NULL IDENTITY(1,1),
    MedicationName NVARCHAR(200) NOT NULL,
    GenericName    NVARCHAR(200)     NULL,
    DosageForm     NVARCHAR(80)      NULL,    -- Tablet, Capsule, Syrup …
    Strength       NVARCHAR(50)      NULL,    -- e.g. '500 mg'
    CONSTRAINT PK_Medications PRIMARY KEY CLUSTERED (MedicationID)
);
GO

CREATE TABLE dbo.Prescriptions (
    PrescriptionID INT           NOT NULL IDENTITY(1,1),
    RecordID       INT           NOT NULL,
    MedicationID   INT           NOT NULL,
    Dosage         NVARCHAR(100) NOT NULL,
    FrequencyPerDay TINYINT      NOT NULL DEFAULT 1,
    DurationDays   INT           NOT NULL DEFAULT 7,
    Instructions   NVARCHAR(300)     NULL,
    CONSTRAINT PK_Prescriptions PRIMARY KEY CLUSTERED (PrescriptionID),
    CONSTRAINT FK_Prescriptions_Records     FOREIGN KEY (RecordID)
        REFERENCES dbo.MedicalRecords (RecordID),
    CONSTRAINT FK_Prescriptions_Medications FOREIGN KEY (MedicationID)
        REFERENCES dbo.Medications    (MedicationID)
);
GO
CREATE NONCLUSTERED INDEX IX_Prescriptions_RecordID ON dbo.Prescriptions (RecordID);
GO

CREATE TABLE dbo.Invoices (
    InvoiceID     INT           NOT NULL IDENTITY(1,1),
    AppointmentID INT           NOT NULL,
    InvoiceDate   DATETIME2     NOT NULL DEFAULT SYSDATETIME(),
    Amount        DECIMAL(10,2) NOT NULL DEFAULT 0.00,
    IsPaid        BIT           NOT NULL DEFAULT 0,
    PaidAt        DATETIME2         NULL,
    CONSTRAINT PK_Invoices PRIMARY KEY CLUSTERED (InvoiceID),
    CONSTRAINT FK_Invoices_Appointments FOREIGN KEY (AppointmentID)
        REFERENCES dbo.Appointments (AppointmentID)
);
GO
CREATE NONCLUSTERED INDEX IX_Invoices_AppointmentID ON dbo.Invoices (AppointmentID);
CREATE NONCLUSTERED INDEX IX_Invoices_IsPaid        ON dbo.Invoices (IsPaid);
GO

-- ---- Views ----

CREATE OR ALTER VIEW dbo.vw_UpcomingAppointments
AS
    SELECT
        a.AppointmentID,
        a.ScheduledAt,
        a.DurationMinutes,
        a.Status,
        p.FirstName + ' ' + p.LastName AS PatientName,
        p.Phone                        AS PatientPhone,
        d.FirstName + ' ' + d.LastName AS DoctorName,
        s.SpecializationName
    FROM dbo.Appointments    a
    JOIN dbo.Patients        p ON p.PatientID = a.PatientID
    JOIN dbo.Doctors         d ON d.DoctorID  = a.DoctorID
    JOIN dbo.Specializations s ON s.SpecializationID = d.SpecializationID
    WHERE a.ScheduledAt >= SYSDATETIME()
      AND a.Status = 'Scheduled';
GO

CREATE OR ALTER VIEW dbo.vw_PatientHistory
AS
    SELECT
        pt.PatientID,
        pt.FirstName + ' ' + pt.LastName   AS PatientName,
        a.AppointmentID,
        a.ScheduledAt,
        d.FirstName + ' ' + d.LastName     AS DoctorName,
        s.SpecializationName,
        mr.Diagnosis,
        mr.Treatment
    FROM dbo.Patients        pt
    JOIN dbo.Appointments    a  ON a.PatientID       = pt.PatientID
    JOIN dbo.Doctors         d  ON d.DoctorID        = a.DoctorID
    JOIN dbo.Specializations s  ON s.SpecializationID = d.SpecializationID
    LEFT JOIN dbo.MedicalRecords mr ON mr.AppointmentID = a.AppointmentID;
GO

CREATE OR ALTER VIEW dbo.vw_UnpaidInvoices
AS
    SELECT
        i.InvoiceID,
        i.InvoiceDate,
        i.Amount,
        p.FirstName + ' ' + p.LastName AS PatientName,
        p.Phone                        AS PatientPhone,
        a.ScheduledAt                  AS AppointmentDate
    FROM dbo.Invoices       i
    JOIN dbo.Appointments   a ON a.AppointmentID = i.AppointmentID
    JOIN dbo.Patients       p ON p.PatientID     = a.PatientID
    WHERE i.IsPaid = 0;
GO

-- ---- Stored Procedures ----

CREATE OR ALTER PROCEDURE dbo.usp_BookAppointment
    @PatientID       INT,
    @DoctorID        INT,
    @ScheduledAt     DATETIME2,
    @DurationMinutes INT           = 30,
    @Reason          NVARCHAR(500) = NULL,
    @NewAppointmentID INT          OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    -- Check for overlapping appointment for the same doctor
    IF EXISTS (
        SELECT 1 FROM dbo.Appointments
        WHERE  DoctorID   = @DoctorID
          AND  Status     = 'Scheduled'
          AND  ScheduledAt < DATEADD(MINUTE, @DurationMinutes, @ScheduledAt)
          AND  DATEADD(MINUTE, DurationMinutes, ScheduledAt) > @ScheduledAt
    )
    BEGIN
        RAISERROR('The doctor already has an overlapping appointment at that time.', 16, 1);
        RETURN;
    END

    INSERT INTO dbo.Appointments (PatientID, DoctorID, ScheduledAt, DurationMinutes, Status, Reason)
    VALUES (@PatientID, @DoctorID, @ScheduledAt, @DurationMinutes, 'Scheduled', @Reason);
    SET @NewAppointmentID = SCOPE_IDENTITY();
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_AddMedicalRecord
    @AppointmentID INT,
    @Diagnosis     NVARCHAR(500) = NULL,
    @Treatment     NVARCHAR(MAX) = NULL,
    @Notes         NVARCHAR(MAX) = NULL,
    @NewRecordID   INT           OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    INSERT INTO dbo.MedicalRecords (AppointmentID, Diagnosis, Treatment, Notes)
    VALUES (@AppointmentID, @Diagnosis, @Treatment, @Notes);
    SET @NewRecordID = SCOPE_IDENTITY();

    -- Mark appointment as Completed
    UPDATE dbo.Appointments
    SET    Status = 'Completed'
    WHERE  AppointmentID = @AppointmentID;
END;
GO

CREATE OR ALTER PROCEDURE dbo.usp_GetDoctorSchedule
    @DoctorID  INT,
    @FromDate  DATE = NULL,
    @ToDate    DATE = NULL
AS
BEGIN
    SET NOCOUNT ON;
    SELECT
        a.AppointmentID,
        a.ScheduledAt,
        a.DurationMinutes,
        a.Status,
        p.FirstName + ' ' + p.LastName AS PatientName,
        a.Reason
    FROM dbo.Appointments a
    JOIN dbo.Patients     p ON p.PatientID = a.PatientID
    WHERE a.DoctorID = @DoctorID
      AND (@FromDate IS NULL OR CAST(a.ScheduledAt AS DATE) >= @FromDate)
      AND (@ToDate   IS NULL OR CAST(a.ScheduledAt AS DATE) <= @ToDate)
    ORDER BY a.ScheduledAt;
END;
GO


-- ============================================================
-- SECTION 5: SQL LOGINS, USERS, AND ROLE ASSIGNMENTS
-- ============================================================
USE [master];
GO

-- Login 1: retail_app_login
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'retail_app_login')
BEGIN
    CREATE LOGIN [retail_app_login]
        WITH PASSWORD   = N'R3t@!lApp#2024!',
             DEFAULT_DATABASE = [RetailShop],
             CHECK_POLICY     = ON,
             CHECK_EXPIRATION = OFF;
END
GO

-- Login 2: hr_app_login
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'hr_app_login')
BEGIN
    CREATE LOGIN [hr_app_login]
        WITH PASSWORD   = N'HR@pp$ecure#2024!',
             DEFAULT_DATABASE = [HRManagement],
             CHECK_POLICY     = ON,
             CHECK_EXPIRATION = OFF;
END
GO

-- Login 3: clinic_app_login
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'clinic_app_login')
BEGIN
    CREATE LOGIN [clinic_app_login]
        WITH PASSWORD   = N'Cl!n!c@pp#2024!',
             DEFAULT_DATABASE = [HealthClinic],
             CHECK_POLICY     = ON,
             CHECK_EXPIRATION = OFF;
END
GO

-- ---- User in RetailShop ----
USE [RetailShop];
GO
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'retail_app_user')
BEGIN
    CREATE USER [retail_app_user] FOR LOGIN [retail_app_login]
        WITH DEFAULT_SCHEMA = dbo;
END
GO
ALTER ROLE [db_owner] ADD MEMBER [retail_app_user];
GO

-- ---- User in HRManagement ----
USE [HRManagement];
GO
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'hr_app_user')
BEGIN
    CREATE USER [hr_app_user] FOR LOGIN [hr_app_login]
        WITH DEFAULT_SCHEMA = dbo;
END
GO
ALTER ROLE [db_owner] ADD MEMBER [hr_app_user];
GO

-- ---- User in HealthClinic ----
USE [HealthClinic];
GO
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'clinic_app_user')
BEGIN
    CREATE USER [clinic_app_user] FOR LOGIN [clinic_app_login]
        WITH DEFAULT_SCHEMA = dbo;
END
GO
ALTER ROLE [db_owner] ADD MEMBER [clinic_app_user];
GO

-- ============================================================
-- END OF SCRIPT
-- ============================================================
